package inspector

import (
	"bytes"
	"context"
	"encoding/hex"
	"reflect"
	"strings"

	"github.com/tokenized/smart-contract/pkg/protocol"
	"github.com/tokenized/smart-contract/pkg/wire"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/pkg/errors"
)

/**
 * Inspector Service
 *
 * What is my purpose?
 * - You look at Bitcoin transactions that I give you
 * - You tell me if they contain return data of interest
 * - You give me back special transaction objects (ITX objects)
 */

var (
	// ErrDecodeFail Failed to decode a transaction payload
	ErrDecodeFail = errors.New("Failed to decode payload")

	// ErrInvalidProtocol The op return data was invalid
	ErrInvalidProtocol = errors.New("Invalid protocol message")

	// ErrMissingInputs
	ErrMissingInputs = errors.New("Message is missing inputs")

	// ErrMissingOutputs
	ErrMissingOutputs = errors.New("Message is missing outputs")

	// targetVersion Protocol prefix
	targetVersion = []byte{0x0, 0x0, 0x0, 0x20}

	// prefixP2PKH Pay to PKH prefix
	prefixP2PKH = []byte{0x76, 0xA9}
)

type NodeInterface interface {
	GetTX(context.Context, *chainhash.Hash) (*wire.MsgTx, error)
}

// NewTransaction builds an ITX from a raw transaction.
func NewTransaction(ctx context.Context, raw string) (*Transaction, error) {
	data := strings.Trim(string(raw), "\n ")

	b, err := hex.DecodeString(data)
	if err != nil {
		return nil, errors.Wrap(ErrDecodeFail, "decoding string")
	}

	// Set up the Wire transaction
	tx := wire.MsgTx{}
	buf := bytes.NewReader(b)

	if err := tx.Deserialize(buf); err != nil {
		return nil, errors.Wrap(ErrDecodeFail, "deserializing wire message")
	}

	return NewTransactionFromWire(ctx, &tx)
}

// NewTransactionFromHash builds an ITX from a transaction hash
func NewTransactionFromHash(ctx context.Context, node NodeInterface, hash string) (*Transaction, error) {
	h, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		return nil, err
	}

	tx, err := node.GetTX(ctx, h)
	if err != nil {
		return nil, err
	}

	return NewTransactionFromWire(ctx, tx)
}

// NewTransactionFromWire builds an ITX from a wire Msg Tx
func NewTransactionFromWire(ctx context.Context, tx *wire.MsgTx) (*Transaction, error) {
	// Must have inputs
	if len(tx.TxIn) == 0 {
		return nil, errors.Wrap(ErrMissingInputs, "parsing transaction")
	}

	// Must have outputs
	if len(tx.TxOut) == 0 {
		return nil, errors.Wrap(ErrMissingOutputs, "parsing transaction")
	}

	// Set up the protocol message
	var msg protocol.OpReturnMessage

	for _, txOut := range tx.TxOut {
		if !isTokenizedOpReturn(txOut.PkScript) {
			continue
		}

		if txOut.PkScript[0] != txscript.OP_RETURN {
			return nil, errors.Wrap(ErrInvalidProtocol, "parsing op return")
		}

		msg, _ = protocol.New(txOut.PkScript)
	}

	return &Transaction{
		Hash:     tx.TxHash(),
		MsgTx:    tx,
		MsgProto: msg,
	}, nil
}

// NewUTXOFromWire returns a new UTXO from a wire message.
func NewUTXOFromWire(tx *wire.MsgTx, index uint32) UTXO {
	return UTXO{
		Hash:     tx.TxHash(),
		Index:    index,
		PkScript: tx.TxOut[index].PkScript,
		Value:    tx.TxOut[index].Value,
	}
}

// Checks if a script carries the protocol signature
func isTokenizedOpReturn(pkScript []byte) bool {
	if len(pkScript) < 20 {
		// This isn't long enough to be a sane message
		return false
	}

	version := make([]byte, 4, 4)

	// Get the version. Where that is, depends on the message structure.
	if pkScript[1] < 0x4c {
		version = pkScript[2:6]
	} else {
		version = pkScript[3:7]
	}

	return bytes.Equal(version, targetVersion)
}

func isPayToPublicKeyHash(pkScript []byte) bool {
	return reflect.DeepEqual(pkScript[0:2], prefixP2PKH)
}