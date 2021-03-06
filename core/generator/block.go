package generator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chainmint/crypto/ed25519"
	"github.com/chainmint/database/pg"
	"github.com/chainmint/database/sql"
	"github.com/chainmint/errors"
	"github.com/chainmint/log"
	"github.com/chainmint/metrics"
	"github.com/chainmint/protocol/bc"
	"github.com/chainmint/protocol/bc/legacy"
	"github.com/chainmint/protocol/state"
	"github.com/chainmint/protocol/vmutil"
)

// errTooFewSigners is returned when a block-signing attempt finds
// that not enough signers are configured for the number of
// signatures required.
var errTooFewSigners = errors.New("too few signers")

var errDuplicateBlock = errors.New("generator already committed to a block at that height")

var (
	once    sync.Once
	latency *metrics.RotatingLatency
)

func recordSince(t0 time.Time) {
	// Lazily publish the expvar and initialize the rotating latency
	// histogram. We don't want to publish metrics that aren't meaningful.
	once.Do(func() {
		latency = metrics.NewRotatingLatency(5, 2*time.Second)
		metrics.PublishLatency("generator.make_block", latency)
	})
	latency.RecordSince(t0)
}

// makeBlock generates a new legacy.Block, collects the required signatures
// and commits the block to the blockchain.
func (g *Generator) MakeBlock(ctx context.Context, time uint64) (error, []byte) {
	latestBlock, latestSnapshot := g.chain.State()
//	var b *legacy.Block
	var s *state.Snapshot

	// Check to see if we already have a pending, generated block.
	// This can happen if the leader process exits between generating
	// the block and committing the signed block to the blockchain.
	b, err := getPendingBlock(ctx, g.db)
	if err != nil {
		return errors.Wrap(err, "retrieving the pending block"), nil
	}
	if b != nil && (latestBlock == nil || b.Height == latestBlock.Height+1) {
		s = state.Copy(latestSnapshot)
		err = s.ApplyBlock(legacy.MapBlock(b))
		if err != nil {
			log.Fatalkv(ctx, log.KeyError, err)
		}
	} else {
		g.mu.Lock()
		txs := g.pool
		g.pool = nil
		g.poolHashes = make(map[bc.Hash]bool)
		g.mu.Unlock()

		b, s, err = g.chain.GenerateBlock(ctx, latestBlock, latestSnapshot, time, txs)
		if err != nil {
			return errors.Wrap(err, "generate"), nil
		}
		if len(b.Transactions) == 0 {
			return nil, b.Hash().Bytes() // don't bother making an empty block
		}
		err = savePendingBlock(ctx, g.db, b)
		if err != nil {
			return errors.Wrap(err, "saving pending block"), nil
		}
	}
	return g.commitBlock(ctx, b, s, latestBlock)
}

func (g *Generator) commitBlock(ctx context.Context, b *legacy.Block, s *state.Snapshot, prevBlock *legacy.Block) (error, []byte) {
	err := g.getAndAddBlockSignatures(ctx, b, prevBlock)
	if err != nil {
		return errors.Wrap(err, "sign"), nil
	}

	err = g.chain.CommitAppliedBlock(ctx, b, s)
	if err != nil {
		return errors.Wrap(err, "commit"), nil
	}
	return nil, b.Hash().Bytes()
}

func (g *Generator) getAndAddBlockSignatures(ctx context.Context, b, prevBlock *legacy.Block) error {
	if prevBlock == nil && b.Height == 1 {
		return nil // no signatures needed for initial block
	}

	pubkeys, quorum, err := vmutil.ParseBlockMultiSigProgram(prevBlock.ConsensusProgram)
	if err != nil {
		return errors.Wrap(err, "parsing prevblock output script")
	}
	if len(g.signers) < quorum {
		return errTooFewSigners
	}

	hashForSig := b.Hash()
	marshalledBlock, err := b.MarshalText()
	if err != nil {
		return errors.Wrap(err, "marshalling block")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	goodSigs := make([][]byte, len(pubkeys))
	replies := make([][]byte, len(g.signers))
	done := make(chan int, len(g.signers))
	for i, signer := range g.signers {
		go getSig(ctx, signer, marshalledBlock, &replies[i], i, done)
	}

	nready := 0
	for i := 0; i < len(g.signers) && nready < quorum; i++ {
		sig := replies[<-done]
		if sig == nil {
			continue
		}
		k := indexKey(pubkeys, hashForSig.Bytes(), sig)
		if k >= 0 && goodSigs[k] == nil {
			goodSigs[k] = sig
			nready++
		} else if k < 0 {
			log.Printkv(ctx, "error", "invalid signature", "block", b.Hash(), "signature", sig)
		}
	}

	if nready < quorum {
		return fmt.Errorf("got %d of %d needed signatures", nready, quorum)
	}
	b.Witness = nonNilSigs(goodSigs)
	return nil
}

func indexKey(keys []ed25519.PublicKey, msg, sig []byte) int {
	for i, key := range keys {
		if ed25519.Verify(key, msg, sig) {
			return i
		}
	}
	return -1
}

func getSig(ctx context.Context, signer BlockSigner, marshalledBlock []byte, sig *[]byte, i int, done chan int) {
	var err error
	*sig, err = signer.SignBlock(ctx, marshalledBlock)
	if err != nil && ctx.Err() != context.Canceled {
		log.Printkv(ctx, "error", err, "signer", signer)
	}
	done <- i
}

func nonNilSigs(a [][]byte) (b [][]byte) {
	for _, p := range a {
		if p != nil {
			b = append(b, p)
		}
	}
	return b
}

// getPendingBlock retrieves the generated, uncommitted block if it exists.
func getPendingBlock(ctx context.Context, db pg.DB) (*legacy.Block, error) {
	const q = `SELECT data FROM generator_pending_block`
	var block legacy.Block
	err := db.QueryRow(ctx, q).Scan(&block)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, errors.Wrap(err, "retrieving generated pending block query")
	}
	return &block, nil
}

// savePendingBlock persists a pending, uncommitted block to the database.
// The generator should save a pending block *before* asking signers to
// sign the block.
func savePendingBlock(ctx context.Context, db pg.DB, b *legacy.Block) error {
	const q = `
		INSERT INTO generator_pending_block (data, height) VALUES($1, $2)
		ON CONFLICT (singleton) DO UPDATE
			SET data = excluded.data, height = excluded.height
			WHERE COALESCE(generator_pending_block.height, 0) < excluded.height
	`
	res, err := db.Exec(ctx, q, b, b.Height)
	if err != nil {
		return errors.Wrap(err, "generator_pending_block insert query")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "generator_pending_block rows affected")
	}
	if affected == 0 {
		return errDuplicateBlock
	}
	return nil
}
