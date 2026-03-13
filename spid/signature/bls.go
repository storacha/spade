package signature

import (
	"fmt"

	filaddr "github.com/filecoin-project/go-address"
	blsgo "github.com/jsign/go-filsigner/bls"
)

type BLSVerifier struct {
	Addr filaddr.Address
}

func (v BLSVerifier) Verify(msg []byte, sig []byte) error {
	sigMatch, err := blsgo.Verify(v.Addr.Payload(), msg, sig)
	if err != nil {
		return fmt.Errorf("BLS signature verification failed: %w", err)
	}
	if !sigMatch {
		return fmt.Errorf("BLS signature verification failed")
	}
	return nil
}

type BLSSigner struct {
	PrivateKey []byte
}

func (v BLSSigner) Sign(msg []byte) ([]byte, error) {
	return blsgo.Sign(v.PrivateKey, msg)
}
