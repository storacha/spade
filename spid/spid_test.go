package spid_test

import (
	"encoding/base64"
	"net/url"
	"testing"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filaddr "github.com/filecoin-project/go-address"
	blsgo "github.com/jsign/go-filsigner/bls"
	"github.com/storacha/spade/spid"
	spid_args "github.com/storacha/spade/spid/args"
	"github.com/storacha/spade/spid/signature"
	"github.com/stretchr/testify/require"
)

func TestSPID(t *testing.T) {
	sp, err := filaddr.NewIDAddress(1000)
	require.NoError(t, err)

	pk, err := base64.StdEncoding.DecodeString("DHdFHAqEZV/DJ8WlHqHkvxyEGqUfOJd78QwrkqkFrp4=")
	require.NoError(t, err)

	workerKey, err := blsgo.GetPubKey(pk)
	require.NoError(t, err)

	argVals := url.Values{}
	argVals.Set("foo", "bar")

	entropy := []byte("test_entropy")

	t.Run("roundtrip", func(t *testing.T) {
		signer := signature.BLSSigner{PrivateKey: pk}

		args, err := spid_args.NewFromValues(signer, entropy, argVals)
		require.NoError(t, err)

		epoch := fil.ClockMainnet.TimeToEpoch(time.Now())

		id, err := spid.New(sp, epoch, args)
		require.NoError(t, err)

		idstr := spid.Format(id)
		t.Logf("Authorization: %s", idstr)

		parsed, err := spid.Parse(idstr)
		require.NoError(t, err)
		require.Equal(t, id, parsed)

		err = spid.Verify(workerKey, entropy, parsed)
		require.NoError(t, err)
	})
}
