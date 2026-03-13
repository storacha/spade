package signature

type Signer interface {
	Sign(msg []byte) ([]byte, error)
}

type Verifier interface {
	Verify(msg []byte, sig []byte) error
}
