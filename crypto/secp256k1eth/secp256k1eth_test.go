//go:build secp256k1eth

package secp256k1eth_test

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcutil/base58"
	underlyingsecp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cometbft/cometbft/v2/crypto"
	"github.com/cometbft/cometbft/v2/crypto/secp256k1eth"
)

type keyData struct {
	priv string
	pub  string
	addr string
}

var secpDataTable = []keyData{
	{
		priv: "a96e62ed3955e65be32703f12d87b6b5cf26039ecfa948dc5107a495418e5330",
		pub:  "04950e1cdfcb133d6024109fd489f734eeb4502418e538c28481f22bce276f248ca0ca66092c9fe8adfbb8424bd92f26e170234c42df756075278ead79a8f5c4ae",
		addr: "1PrkgVnuHLGZu4EUQGmXkGVuhTfn7t8DJK",
	},
}

func TestPubKeySecp256k1EthAddress(t *testing.T) {
	for _, d := range secpDataTable {
		privB, _ := hex.DecodeString(d.priv)
		pubB, _ := hex.DecodeString(d.pub)
		addrBbz, _, _ := base58.CheckDecode(d.addr)
		addrB := crypto.Address(addrBbz)

		priv := secp256k1eth.PrivKey(privB)

		pubKey := priv.PubKey()
		pubT, _ := pubKey.(secp256k1eth.PubKey)
		pub := pubT
		addr := pubKey.Address()

		assert.Equal(t, pub, secp256k1eth.PubKey(pubB), "Expected pub keys to match")
		assert.Equal(t, addr, addrB, "Expected addresses to match")
	}
}

func TestSignAndValidateSecp256k1Eth(t *testing.T) {
	privKey := secp256k1eth.GenPrivKey()
	pubKey := privKey.PubKey()

	msg := crypto.CRandBytes(128)
	sig, err := privKey.Sign(msg)
	require.NoError(t, err)

	assert.True(t, pubKey.VerifySignature(msg, sig))

	// Mutate the signature, just one bit.
	sig[3] ^= byte(0x01)

	assert.False(t, pubKey.VerifySignature(msg, sig))
}

// This test is intended to justify the removal of calls to the underlying library
// in creating the privkey.
func TestSecp256k1LoadPrivkeyAndSerializeIsIdentity(t *testing.T) {
	numberOfTests := 256
	for i := 0; i < numberOfTests; i++ {
		// Seed the test case with some random bytes
		privKeyBytes := [32]byte{}
		copy(privKeyBytes[:], crypto.CRandBytes(32))

		// This function creates a private and public key in the underlying libraries format.
		// The private key is basically calling new(big.Int).SetBytes(pk), which removes leading zero bytes
		// TODO Deal with this
		priv := underlyingsecp256k1.PrivKeyFromBytes(privKeyBytes[:])
		// this takes the bytes returned by `(big int).Bytes()`, and if the length is less than 32 bytes,
		// pads the bytes from the left with zero bytes. Therefore these two functions composed
		// result in the identity function on privKeyBytes, hence the following equality check
		// always returning true.
		serializedBytes := priv.Serialize()
		require.Equal(t, privKeyBytes[:], serializedBytes)
	}
}

func TestGenPrivKeySecp256k1Eth(t *testing.T) {
	// curve order n
	n := underlyingsecp256k1.S256().N
	tests := []struct {
		name   string
		secret []byte
	}{
		{"empty secret", []byte{}},
		{
			"some long secret",
			[]byte("We live in a society exquisitely dependent on science and technology, " +
				"in which hardly anyone knows anything about science and technology."),
		},
		{"another seed used in cosmos tests #1", []byte{0}},
		{"another seed used in cosmos tests #2", []byte("mySecret")},
		{"another seed used in cosmos tests #3", []byte("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// TODO Deal with this
			gotPrivKey := secp256k1eth.GenPrivKeySecp256k1(tt.secret)
			require.NotNil(t, gotPrivKey)
			// interpret as a big.Int and make sure it is a valid field element:
			fe := new(big.Int).SetBytes(gotPrivKey[:])
			require.Less(t, fe.Cmp(n), 0)
			require.Greater(t, fe.Sign(), 0)
		})
	}
}
