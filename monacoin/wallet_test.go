package monacoin

import (
	"crypto/rand"
	"github.com/OpenBazaar/multiwallet/datastore"
	"github.com/OpenBazaar/multiwallet/keys"
	"github.com/OpenBazaar/wallet-interface"
	"github.com/monasuite/monad/chaincfg"
	"github.com/monasuite/monautil/hdkeychain"
	"strings"
	"testing"
)

func TestMonacoinWallet_CurrentAddress(t *testing.T) {
	w, seed, err := createWalletAndSeed()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		addr := w.CurrentAddress(wallet.EXTERNAL)
		if strings.HasPrefix(strings.ToLower(addr.String()), "ltc1") {
			t.Errorf("Address %s hash ltc1 prefix: seed %x", addr, seed)
		}
		if err := w.db.Keys().MarkKeyAsUsed(addr.ScriptAddress()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMonacoinWallet_NewAddress(t *testing.T) {
	w, seed, err := createWalletAndSeed()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		addr := w.NewAddress(wallet.EXTERNAL)
		if strings.HasPrefix(strings.ToLower(addr.String()), "ltc1") {
			t.Errorf("Address %s hash ltc1 prefix: %x", addr, seed)
		}
	}
}

func createWalletAndSeed() (*MonacoinWallet, []byte, error) {
	ds := datastore.NewMockMultiwalletDatastore()
	db, err := ds.GetDatastoreForWallet(wallet.Monacoin)
	if err != nil {
		return nil, nil, err
	}

	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, nil, err
	}

	masterPrivKey, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, nil, err
	}
	km, err := keys.NewKeyManager(db.Keys(), &chaincfg.MainNetParams, masterPrivKey, wallet.Monacoin, MonacoinAddress)
	if err != nil {
		return nil, nil, err
	}

	return &MonacoinWallet{
		db:     db,
		km:     km,
		params: &chaincfg.MainNetParams,
	}, seed, nil
}
