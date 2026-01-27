package args

import (
	"fmt"
	"net/url"

	"github.com/storacha/spade/spid/signature"
)

// Args are optional signed SPID authorization arguments.
type Args struct {
	data []byte
	sig  []byte
}

// Raw retrieves the raw argument bytes.
func (a Args) Raw() []byte {
	return a.data
}

func (a Args) Signature() []byte {
	return a.sig
}

// Values parses the argument bytes as URL-encoded query values.
func (a Args) Values() (url.Values, error) {
	v, err := url.ParseQuery(string(a.data))
	if err != nil {
		return nil, fmt.Errorf("parsing arguments as URL query: %w", err)
	}
	return v, nil
}

func New(data, sig []byte) Args {
	return Args{data: data, sig: sig}
}

// NewFromValues creates signed [Args] from URL values and signed using the
// provided signer and drand randomness beacon data.
func NewFromValues(signer signature.Signer, entropy []byte, values url.Values) (Args, error) {
	msg := []byte(values.Encode())
	sig, err := Sign(signer, entropy, msg)
	if err != nil {
		return Args{}, err
	}
	return New(msg, sig), nil
}

func SignaturePayload(entropy []byte, data []byte) []byte {
	return append(append([]byte{0x20, 0x20, 0x20}, entropy...), data...)
}

func Sign(signer signature.Signer, entropy []byte, data []byte) ([]byte, error) {
	return signer.Sign(SignaturePayload(entropy, data))
}

func Verify(verifier signature.Verifier, entropy []byte, args Args) error {
	return verifier.Verify(SignaturePayload(entropy, args.Raw()), args.Signature())
}
