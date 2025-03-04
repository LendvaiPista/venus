package internal

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tf "github.com/filecoin-project/venus/pkg/testhelpers/testflags"

	_ "github.com/filecoin-project/venus/pkg/crypto/bls"
	_ "github.com/filecoin-project/venus/pkg/crypto/secp"
)

var mockSigner = NewMockSigner(MustGenerateKeyInfo(1, 42))

func TestSignedMessageString(t *testing.T) {
	tf.UnitTest(t)

	smsg := makeMessage(t, mockSigner, 42)
	cid := smsg.Cid()
	got := smsg.String()
	assert.Contains(t, got, cid.String())
}

func TestSignedMessageMarshal(t *testing.T) {
	tf.UnitTest(t)

	smsg := makeMessage(t, mockSigner, 42)

	marshalled := new(bytes.Buffer)
	err := smsg.MarshalCBOR(marshalled)
	assert.NoError(t, err)

	smsgBack := SignedMessage{}
	assert.False(t, smsg.Equals(&smsgBack))

	err = smsgBack.UnmarshalCBOR(marshalled)
	assert.NoError(t, err)

	assert.Equal(t, smsg.Message, smsgBack.Message)
	assert.Equal(t, smsg.Signature, smsgBack.Signature)
	assert.True(t, smsg.Equals(&smsgBack))
}

func TestSignedMessageCid(t *testing.T) {
	tf.UnitTest(t)

	smsg1 := makeMessage(t, mockSigner, 41)
	smsg2 := makeMessage(t, mockSigner, 42)

	c1 := smsg1.Cid()
	c2 := smsg2.Cid()

	assert.NotEqual(t, c1.String(), c2.String())

}

func TestSignedMessageCidToNode(t *testing.T) {
	tf.UnitTest(t)

	smsg := makeMessage(t, mockSigner, 41)

	c := smsg.Cid()
	n, err := smsg.ToNode()
	require.NoError(t, err)

	assert.Equal(t, c, n.Cid())

}

func makeMessage(t *testing.T, signer MockSigner, nonce uint64) *SignedMessage {
	newAddr, err := address.NewSecp256k1Address([]byte("receiver"))
	require.NoError(t, err)

	msg := NewMeteredMessage(
		signer.Addresses[0],
		newAddr,
		nonce,
		NewAttoFILFromFIL(2),
		abi.MethodNum(2352),
		[]byte("params"),
		NewGasFeeCap(1000),
		NewGasPremium(100),
		100)
	smsg, err := NewSignedMessage(context.TODO(), *msg, &signer)
	require.NoError(t, err)

	// This check requests that you add a non-zero value for new fields above,
	// then update the field count below.
	require.Equal(t, 2, reflect.TypeOf(*smsg).NumField())

	return smsg
}
