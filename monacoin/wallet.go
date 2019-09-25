package monacoin

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/OpenBazaar/multiwallet/cache"
	"github.com/OpenBazaar/multiwallet/client"
	"github.com/OpenBazaar/multiwallet/config"
	"github.com/OpenBazaar/multiwallet/keys"
	maddr "github.com/OpenBazaar/multiwallet/Monacoin/address"
	"github.com/OpenBazaar/multiwallet/model"
	"github.com/OpenBazaar/multiwallet/service"
	"github.com/OpenBazaar/multiwallet/util"
	wi "github.com/OpenBazaar/wallet-interface"
	"github.com/monasuite/monad/chaincfg"
	"github.com/monasuite/monad/chaincfg/chainhash"
	"github.com/monasuite/monad/wire"
	"github.com/monasuite/monautil"
	hd "github.com/monasuite/monautil/hdkeychain"
	"github.com/monasuite/ltcutil"
	"github.com/monasuite/monawallet/wallet/txrules"
	logging "github.com/op/go-logging"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/net/proxy"
)

type MonacoinWallet struct {
	db     wi.Datastore
	km     *keys.KeyManager
	params *chaincfg.Params
	client model.APIClient
	ws     *service.WalletService
	fp     *util.FeeProvider

	mPrivKey *hd.ExtendedKey
	mPubKey  *hd.ExtendedKey

	exchangeRates wi.ExchangeRates
	log           *logging.Logger
}

func NewMonacoinWallet(cfg config.CoinConfig, mnemonic string, params *chaincfg.Params, proxy proxy.Dialer, cache cache.Cacher, disableExchangeRates bool) (*MonacoinWallet, error) {
	seed := bip39.NewSeed(mnemonic, "")

	mPrivKey, err := hd.NewMaster(seed, params)
	if err != nil {
		return nil, err
	}
	mPubKey, err := mPrivKey.Neuter()
	if err != nil {
		return nil, err
	}
	km, err := keys.NewKeyManager(cfg.DB.Keys(), params, mPrivKey, wi.Monacoin, MonacoinAddress)
	if err != nil {
		return nil, err
	}

	c, err := client.NewClientPool(cfg.ClientAPIs, proxy)
	if err != nil {
		return nil, err
	}

	wm, err := service.NewWalletService(cfg.DB, km, c, params, wi.Monacoin, cache)
	if err != nil {
		return nil, err
	}
	var er wi.ExchangeRates
	if !disableExchangeRates {
		er = NewMonacoinPriceFetcher(proxy)
	}

	fp := util.NewFeeProvider(cfg.MaxFee, cfg.HighFee, cfg.MediumFee, cfg.LowFee, er)

	return &MonacoinWallet{
		db:            cfg.DB,
		km:            km,
		params:        params,
		client:        c,
		ws:            wm,
		fp:            fp,
		mPrivKey:      mPrivKey,
		mPubKey:       mPubKey,
		exchangeRates: er,
		log:           logging.MustGetLogger("Monacoin-wallet"),
	}, nil
}

func MonacoinAddress(key *hd.ExtendedKey, params *chaincfg.Params) (monautil.Address, error) {
	addr, err := key.Address(params)
	if err != nil {
		return nil, err
	}
	return laddr.NewAddressPubKeyHash(addr.ScriptAddress(), params)
}
func (w *MonacoinWallet) Start() {
	w.client.Start()
	w.ws.Start()
}

func (w *MonacoinWallet) Params() *chaincfg.Params {
	return w.params
}

func (w *MonacoinWallet) CurrencyCode() string {
	if w.params.Name == chaincfg.MainNetParams.Name {
		return "ltc"
	} else {
		return "tltc"
	}
}

func (w *MonacoinWallet) IsDust(amount int64) bool {
	return txrules.IsDustAmount(ltcutil.Amount(amount), 25, txrules.DefaultRelayFeePerKb)
}

func (w *MonacoinWallet) MasterPrivateKey() *hd.ExtendedKey {
	return w.mPrivKey
}

func (w *MonacoinWallet) MasterPublicKey() *hd.ExtendedKey {
	return w.mPubKey
}

func (w *MonacoinWallet) ChildKey(keyBytes []byte, chaincode []byte, isPrivateKey bool) (*hd.ExtendedKey, error) {
	parentFP := []byte{0x00, 0x00, 0x00, 0x00}
	var id []byte
	if isPrivateKey {
		id = w.params.HDPrivateKeyID[:]
	} else {
		id = w.params.HDPublicKeyID[:]
	}
	hdKey := hd.NewExtendedKey(
		id,
		keyBytes,
		chaincode,
		parentFP,
		0,
		0,
		isPrivateKey)
	return hdKey.Child(0)
}

func (w *MonacoinWallet) CurrentAddress(purpose wi.KeyPurpose) monautil.Address {
	var addr monautil.Address
	for {
		key, err := w.km.GetCurrentKey(purpose)
		if err != nil {
			w.log.Errorf("Error generating current key: %s", err)
		}
		addr, err = w.km.KeyToAddress(key)
		if err != nil {
			w.log.Errorf("Error converting key to address: %s", err)
		}

		if !strings.HasPrefix(strings.ToLower(addr.String()), "ltc1") {
			break
		}
		if err := w.db.Keys().MarkKeyAsUsed(addr.ScriptAddress()); err != nil {
			w.log.Errorf("Error marking key as used: %s", err)
		}
	}
	return addr
}

func (w *MonacoinWallet) NewAddress(purpose wi.KeyPurpose) monautil.Address {
	var addr monautil.Address
	for {
		key, err := w.km.GetNextUnused(purpose)
		if err != nil {
			w.log.Errorf("Error generating next unused key: %s", err)
		}
		addr, err = w.km.KeyToAddress(key)
		if err != nil {
			w.log.Errorf("Error converting key to address: %s", err)
		}
		if err := w.db.Keys().MarkKeyAsUsed(addr.ScriptAddress()); err != nil {
			w.log.Errorf("Error marking key as used: %s", err)
		}
		if !strings.HasPrefix(strings.ToLower(addr.String()), "ltc1") {
			break
		}
	}
	return addr
}

func (w *MonacoinWallet) DecodeAddress(addr string) (monautil.Address, error) {
	return laddr.DecodeAddress(addr, w.params)
}

func (w *MonacoinWallet) ScriptToAddress(script []byte) (monautil.Address, error) {
	return laddr.ExtractPkScriptAddrs(script, w.params)
}

func (w *MonacoinWallet) AddressToScript(addr monautil.Address) ([]byte, error) {
	return laddr.PayToAddrScript(addr)
}

func (w *MonacoinWallet) HasKey(addr monautil.Address) bool {
	_, err := w.km.GetKeyForScript(addr.ScriptAddress())
	return err == nil
}

func (w *MonacoinWallet) Balance() (confirmed, unconfirmed int64) {
	utxos, _ := w.db.Utxos().GetAll()
	txns, _ := w.db.Txns().GetAll(false)
	return util.CalcBalance(utxos, txns)
}

func (w *MonacoinWallet) Transactions() ([]wi.Txn, error) {
	height, _ := w.ChainTip()
	txns, err := w.db.Txns().GetAll(false)
	if err != nil {
		return txns, err
	}
	for i, tx := range txns {
		var confirmations int32
		var status wi.StatusCode
		confs := int32(height) - tx.Height + 1
		if tx.Height <= 0 {
			confs = tx.Height
		}
		switch {
		case confs < 0:
			status = wi.StatusDead
		case confs == 0 && time.Since(tx.Timestamp) <= time.Hour*6:
			status = wi.StatusUnconfirmed
		case confs == 0 && time.Since(tx.Timestamp) > time.Hour*6:
			status = wi.StatusDead
		case confs > 0 && confs < 24:
			status = wi.StatusPending
			confirmations = confs
		case confs > 23:
			status = wi.StatusConfirmed
			confirmations = confs
		}
		tx.Confirmations = int64(confirmations)
		tx.Status = status
		txns[i] = tx
	}
	return txns, nil
}

func (w *MonacoinWallet) GetTransaction(txid chainhash.Hash) (wi.Txn, error) {
	txn, err := w.db.Txns().Get(txid)
	if err == nil {
		tx := wire.NewMsgTx(1)
		rbuf := bytes.NewReader(txn.Bytes)
		err := tx.BtcDecode(rbuf, wire.ProtocolVersion, wire.WitnessEncoding)
		if err != nil {
			return txn, err
		}
		outs := []wi.TransactionOutput{}
		for i, out := range tx.TxOut {
			addr, err := laddr.ExtractPkScriptAddrs(out.PkScript, w.params)
			if err != nil {
				w.log.Errorf("error extracting address from txn pkscript: %v\n", err)
			}
			tout := wi.TransactionOutput{
				Address: addr,
				Value:   out.Value,
				Index:   uint32(i),
			}
			outs = append(outs, tout)
		}
		txn.Outputs = outs
	}
	return txn, err
}

func (w *MonacoinWallet) ChainTip() (uint32, chainhash.Hash) {
	return w.ws.ChainTip()
}

func (w *MonacoinWallet) GetFeePerByte(feeLevel wi.FeeLevel) uint64 {
	return w.fp.GetFeePerByte(feeLevel)
}

func (w *MonacoinWallet) Spend(amount int64, addr monautil.Address, feeLevel wi.FeeLevel, referenceID string, spendAll bool) (*chainhash.Hash, error) {
	var (
		tx  *wire.MsgTx
		err error
	)
	if spendAll {
		tx, err = w.buildSpendAllTx(addr, feeLevel)
		if err != nil {
			return nil, err
		}
	} else {
		tx, err = w.buildTx(amount, addr, feeLevel, nil)
		if err != nil {
			return nil, err
		}
	}

	// Broadcast
	if err := w.Broadcast(tx); err != nil {
		return nil, err
	}

	ch := tx.TxHash()
	return &ch, nil
}

func (w *MonacoinWallet) BumpFee(txid chainhash.Hash) (*chainhash.Hash, error) {
	return w.bumpFee(txid)
}

func (w *MonacoinWallet) EstimateFee(ins []wi.TransactionInput, outs []wi.TransactionOutput, feePerByte uint64) uint64 {
	tx := new(wire.MsgTx)
	for _, out := range outs {
		scriptPubKey, _ := laddr.PayToAddrScript(out.Address)
		output := wire.NewTxOut(out.Value, scriptPubKey)
		tx.TxOut = append(tx.TxOut, output)
	}
	estimatedSize := EstimateSerializeSize(len(ins), tx.TxOut, false, P2PKH)
	fee := estimatedSize * int(feePerByte)
	return uint64(fee)
}

func (w *MonacoinWallet) EstimateSpendFee(amount int64, feeLevel wi.FeeLevel) (uint64, error) {
	return w.estimateSpendFee(amount, feeLevel)
}

func (w *MonacoinWallet) SweepAddress(ins []wi.TransactionInput, address *monautil.Address, key *hd.ExtendedKey, redeemScript *[]byte, feeLevel wi.FeeLevel) (*chainhash.Hash, error) {
	return w.sweepAddress(ins, address, key, redeemScript, feeLevel)
}

func (w *MonacoinWallet) CreateMultisigSignature(ins []wi.TransactionInput, outs []wi.TransactionOutput, key *hd.ExtendedKey, redeemScript []byte, feePerByte uint64) ([]wi.Signature, error) {
	return w.createMultisigSignature(ins, outs, key, redeemScript, feePerByte)
}

func (w *MonacoinWallet) Multisign(ins []wi.TransactionInput, outs []wi.TransactionOutput, sigs1 []wi.Signature, sigs2 []wi.Signature, redeemScript []byte, feePerByte uint64, broadcast bool) ([]byte, error) {
	return w.multisign(ins, outs, sigs1, sigs2, redeemScript, feePerByte, broadcast)
}

func (w *MonacoinWallet) GenerateMultisigScript(keys []hd.ExtendedKey, threshold int, timeout time.Duration, timeoutKey *hd.ExtendedKey) (addr monautil.Address, redeemScript []byte, err error) {
	return w.generateMultisigScript(keys, threshold, timeout, timeoutKey)
}

func (w *MonacoinWallet) AddWatchedAddress(addr monautil.Address) error {
	script, err := w.AddressToScript(addr)
	if err != nil {
		return err
	}
	err = w.db.WatchedScripts().Put(script)
	if err != nil {
		return err
	}
	w.client.ListenAddress(addr)
	return nil
}

func (w *MonacoinWallet) AddWatchedScript(script []byte) error {
	err := w.db.WatchedScripts().Put(script)
	if err != nil {
		return err
	}
	addr, err := w.ScriptToAddress(script)
	if err != nil {
		return err
	}
	w.client.ListenAddress(addr)
	return nil
}

func (w *MonacoinWallet) AddTransactionListener(callback func(wi.TransactionCallback)) {
	w.ws.AddTransactionListener(callback)
}

func (w *MonacoinWallet) ReSyncBlockchain(fromTime time.Time) {
	go w.ws.UpdateState()
}

func (w *MonacoinWallet) GetConfirmations(txid chainhash.Hash) (uint32, uint32, error) {
	txn, err := w.db.Txns().Get(txid)
	if err != nil {
		return 0, 0, err
	}
	if txn.Height == 0 {
		return 0, 0, nil
	}
	chainTip, _ := w.ChainTip()
	return chainTip - uint32(txn.Height) + 1, uint32(txn.Height), nil
}

func (w *MonacoinWallet) Close() {
	w.ws.Stop()
	w.client.Close()
}

func (w *MonacoinWallet) ExchangeRates() wi.ExchangeRates {
	return w.exchangeRates
}

func (w *MonacoinWallet) DumpTables(wr io.Writer) {
	fmt.Fprintln(wr, "Transactions-----")
	txns, _ := w.db.Txns().GetAll(true)
	for _, tx := range txns {
		fmt.Fprintf(wr, "Hash: %s, Height: %d, Value: %d, WatchOnly: %t\n", tx.Txid, int(tx.Height), int(tx.Value), tx.WatchOnly)
	}
	fmt.Fprintln(wr, "\nUtxos-----")
	utxos, _ := w.db.Utxos().GetAll()
	for _, u := range utxos {
		fmt.Fprintf(wr, "Hash: %s, Index: %d, Height: %d, Value: %d, WatchOnly: %t\n", u.Op.Hash.String(), int(u.Op.Index), int(u.AtHeight), int(u.Value), u.WatchOnly)
	}
	fmt.Fprintln(wr, "\nKeys-----")
	keys, _ := w.db.Keys().GetAll()
	unusedInternal, _ := w.db.Keys().GetUnused(wi.INTERNAL)
	unusedExternal, _ := w.db.Keys().GetUnused(wi.EXTERNAL)
	internalMap := make(map[int]bool)
	externalMap := make(map[int]bool)
	for _, k := range unusedInternal {
		internalMap[k] = true
	}
	for _, k := range unusedExternal {
		externalMap[k] = true
	}

	for _, k := range keys {
		var used bool
		if k.Purpose == wi.INTERNAL {
			used = internalMap[k.Index]
		} else {
			used = externalMap[k.Index]
		}
		fmt.Fprintf(wr, "KeyIndex: %d, Purpose: %d, Used: %t\n", k.Index, k.Purpose, used)
	}
}

// Build a client.Transaction so we can ingest it into the wallet service then broadcast
func (w *MonacoinWallet) Broadcast(tx *wire.MsgTx) error {
	var buf bytes.Buffer
	tx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding)
	cTxn := model.Transaction{
		Txid:          tx.TxHash().String(),
		Locktime:      int(tx.LockTime),
		Version:       int(tx.Version),
		Confirmations: 0,
		Time:          time.Now().Unix(),
		RawBytes:      buf.Bytes(),
	}
	utxos, err := w.db.Utxos().GetAll()
	if err != nil {
		return err
	}
	for n, in := range tx.TxIn {
		var u wi.Utxo
		for _, ut := range utxos {
			if util.OutPointsEqual(ut.Op, in.PreviousOutPoint) {
				u = ut
				break
			}
		}
		addr, err := w.ScriptToAddress(u.ScriptPubkey)
		if err != nil {
			return err
		}
		input := model.Input{
			Txid: in.PreviousOutPoint.Hash.String(),
			Vout: int(in.PreviousOutPoint.Index),
			ScriptSig: model.Script{
				Hex: hex.EncodeToString(in.SignatureScript),
			},
			Sequence: uint32(in.Sequence),
			N:        n,
			Addr:     addr.String(),
			Satoshis: u.Value,
			Value:    float64(u.Value) / util.SatoshisPerCoin(wi.Monacoin),
		}
		cTxn.Inputs = append(cTxn.Inputs, input)
	}
	for n, out := range tx.TxOut {
		addr, err := w.ScriptToAddress(out.PkScript)
		if err != nil {
			return err
		}
		output := model.Output{
			N: n,
			ScriptPubKey: model.OutScript{
				Script: model.Script{
					Hex: hex.EncodeToString(out.PkScript),
				},
				Addresses: []string{addr.String()},
			},
			Value: float64(float64(out.Value) / util.SatoshisPerCoin(wi.Bitcoin)),
		}
		cTxn.Outputs = append(cTxn.Outputs, output)
	}
	_, err = w.client.Broadcast(buf.Bytes())
	if err != nil {
		return err
	}
	w.ws.ProcessIncomingTransaction(cTxn)
	return nil
}

// AssociateTransactionWithOrder used for ORDER_PAYMENT message
func (w *MonacoinWallet) AssociateTransactionWithOrder(cb wi.TransactionCallback) {
	w.ws.InvokeTransactionListeners(cb)
}