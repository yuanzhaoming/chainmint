package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/chainmint/crypto/ed25519/chainkd"
	"github.com/chainmint/database/pg/pgtest"
	"github.com/chainmint/errors"
	"github.com/chainmint/protocol/bc"
	"github.com/chainmint/protocol/prottest"
	"github.com/chainmint/protocol/vm"
	"github.com/chainmint/testutil"
)

func TestCreateAccount(t *testing.T) {
	db := pgtest.NewTx(t)
	m := NewManager(db, prottest.NewChain(t), nil)
	ctx := context.Background()

	account, err := m.Create(ctx, []chainkd.XPub{testutil.TestXPub}, 1, "", nil, "")
	if err != nil {
		testutil.FatalErr(t, err)
	}

	// Verify that the account was defined.
	var id string
	var checkQ = `SELECT id FROM signers`
	err = m.db.QueryRow(ctx, checkQ).Scan(&id)
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	if id != account.ID {
		t.Errorf("expected account %s to be recorded as %s", account.ID, id)
	}
}

func TestCreateAccountIdempotency(t *testing.T) {
	db := pgtest.NewTx(t)
	m := NewManager(db, prottest.NewChain(t), nil)
	ctx := context.Background()
	var clientToken = "a-unique-client-token"

	account1, err := m.Create(ctx, []chainkd.XPub{testutil.TestXPub}, 1, "satoshi", nil, clientToken)
	if err != nil {
		testutil.FatalErr(t, err)
	}
	account2, err := m.Create(ctx, []chainkd.XPub{testutil.TestXPub}, 1, "satoshi", nil, clientToken)
	if err != nil {
		testutil.FatalErr(t, err)
	}
	if !testutil.DeepEqual(account1, account2) {
		t.Errorf("got=%#v, want=%#v", account2, account1)
	}
}

func TestCreateAccountReusedAlias(t *testing.T) {
	db := pgtest.NewTx(t)
	m := NewManager(db, prottest.NewChain(t), nil)
	ctx := context.Background()
	m.createTestAccount(ctx, t, "some-account", nil)

	_, err := m.Create(ctx, []chainkd.XPub{testutil.TestXPub}, 1, "some-account", nil, "")
	if errors.Root(err) != ErrDuplicateAlias {
		t.Errorf("Expected %s when reusing an alias, got %v", ErrDuplicateAlias, err)
	}
}

func TestCreateControlProgram(t *testing.T) {
	// use pgtest.NewDB for deterministic postgres sequences
	_, db := pgtest.NewDB(t, pgtest.SchemaPath)
	m := NewManager(db, prottest.NewChain(t), nil)
	ctx := context.Background()

	account, err := m.Create(ctx, []chainkd.XPub{testutil.TestXPub}, 1, "", nil, "")
	if err != nil {
		testutil.FatalErr(t, err)
	}

	got, err := m.CreateControlProgram(ctx, account.ID, false, time.Now().Add(5*time.Minute))
	if err != nil {
		testutil.FatalErr(t, err)
	}

	want, err := vm.Assemble("DUP TOALTSTACK SHA3 0x6dbfeed3d0cffddbda105bfe320072b067304af099c9cff0251d5446412e524a 1 1 CHECKMULTISIG VERIFY FROMALTSTACK 0 CHECKPREDICATE")
	if err != nil {
		testutil.FatalErr(t, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("got control program = %x want %x", got, want)
	}
}

func (m *Manager) createTestAccount(ctx context.Context, t testing.TB, alias string, tags map[string]interface{}) *Account {
	account, err := m.Create(ctx, []chainkd.XPub{testutil.TestXPub}, 1, alias, tags, "")
	if err != nil {
		testutil.FatalErr(t, err)
	}

	return account
}

func (m *Manager) createTestControlProgram(ctx context.Context, t testing.TB, accountID string) *controlProgram {
	if accountID == "" {
		account := m.createTestAccount(ctx, t, "", nil)
		accountID = account.ID
	}

	cp, err := m.createControlProgram(ctx, accountID, false, time.Time{})
	if err != nil {
		testutil.FatalErr(t, err)
	}
	err = m.insertAccountControlProgram(ctx, cp)
	if err != nil {
		testutil.FatalErr(t, err)
	}
	return cp
}

func randHash() (h bc.Hash) {
	h.ReadFrom(rand.Reader)
	return h
}

func (m *Manager) createTestUTXO(ctx context.Context, t testing.TB, accountID string) bc.Hash {
	if accountID == "" {
		accountID = m.createTestAccount(ctx, t, "", nil).ID
	}

	// Create an account control program for the new UTXO.
	cp := m.createTestControlProgram(ctx, t, accountID)

	outputID := randHash()
	const q = `
		INSERT INTO account_utxos (asset_id, amount, account_id,
		control_program_index, control_program, confirmed_in,
		output_id, source_id, source_pos, ref_data_hash, change)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, false)
	`
	_, err := m.db.Exec(ctx, q, randHash(), 100, accountID,
		cp.keyIndex, cp.controlProgram, 10, outputID, randHash(), 0, randHash())
	if err != nil {
		testutil.FatalErr(t, err)
	}
	return outputID
}

func TestFindByID(t *testing.T) {
	db := pgtest.NewTx(t)
	m := NewManager(db, prottest.NewChain(t), nil)
	ctx := context.Background()
	account := m.createTestAccount(ctx, t, "", nil)

	found, err := m.findByID(ctx, account.ID)
	if err != nil {
		testutil.FatalErr(t, err)
	}

	if !testutil.DeepEqual(account.Signer, found) {
		t.Errorf("expected found account to be %v, instead found %v", account, found)
	}
}

func TestFindByAlias(t *testing.T) {
	db := pgtest.NewTx(t)
	m := NewManager(db, prottest.NewChain(t), nil)
	ctx := context.Background()
	account := m.createTestAccount(ctx, t, "some-alias", nil)

	found, err := m.FindByAlias(ctx, "some-alias")
	if err != nil {
		testutil.FatalErr(t, err)
	}

	if !testutil.DeepEqual(account.Signer, found) {
		t.Errorf("expected found account to be %v, instead found %v", account, found)
	}
}