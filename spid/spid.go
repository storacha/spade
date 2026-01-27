package spid

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filaddr "github.com/filecoin-project/go-address"
	filabi "github.com/filecoin-project/go-state-types/abi"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/storacha/spade/spid/args"
	"github.com/storacha/spade/spid/signature"
)

const (
	Scheme         = `FIL-SPID-V0`
	SigGraceEpochs = 5 // 30 secs per epoch
)

var (
	spAuthRe = regexp.MustCompile(
		`^` + Scheme + `\s+` +
			// fil epoch
			`([0-9]+)` + `\s*;\s*` +
			// spID
			`([ft]0[0-9]+)` + `\s*;\s*` +
			// signature
			`([^; ]+)` +
			// optional signed argument
			`(?:\s*\;\s*([^; ]+))?` +
			`\s*$`,
	)
	challengeCache, _ = lru.New[string, verifySigResult](SigGraceEpochs * 128)
)

type verifySigResult struct {
	invalidSigErr error
}

type rawHeader struct {
	epoch  string
	addr   string
	sigB64 string
	arg    string
}

// Challenge is a parsed SPID authorization that can be verified.
type Challenge struct {
	epoch filabi.ChainEpoch // Filecoin epoch
	addr  filaddr.Address   // storage provider address
	args  args.Args         // decoded signed args
}

func New(addr filaddr.Address, epoch filabi.ChainEpoch, args args.Args) (Challenge, error) {
	return Challenge{addr: addr, epoch: epoch, args: args}, nil
}

func (c Challenge) Addr() filaddr.Address {
	return c.addr
}

func (c Challenge) Epoch() filabi.ChainEpoch {
	return c.epoch
}

// Args retrieves the signed arguments.
func (c Challenge) Args() args.Args {
	return c.args
}

func (c Challenge) String() string {
	return Format(c)
}

// Parse parses the provided Authorization header string as a SPID Challenge.
// Note: argument signature verification is not performed here.
func Parse(s string) (Challenge, error) {
	var challenge Challenge
	res := spAuthRe.FindStringSubmatch(s)

	if len(res) != 4 && len(res) != 5 {
		return Challenge{}, fmt.Errorf("invalid %s auth header: %q", Scheme, s)
	}

	hdr := rawHeader{}
	hdr.epoch, hdr.addr, hdr.sigB64 = res[1], res[2], res[3]
	if len(res) == 5 { // allow for optional argument
		hdr.arg = res[4]
	}

	var err error
	challenge.addr, err = filaddr.NewFromString(hdr.addr)
	if err != nil {
		return Challenge{}, fmt.Errorf("unexpected %s auth address: %q", Scheme, hdr.addr)
	}

	e, err := strconv.ParseInt(hdr.epoch, 10, 32)
	if err != nil {
		return Challenge{}, fmt.Errorf("unexpected %s auth epoch: %s", Scheme, hdr.epoch)
	}
	challenge.epoch = filabi.ChainEpoch(e)

	curFilEpoch := fil.ClockMainnet.TimeToEpoch(time.Now())
	if curFilEpoch < challenge.epoch {
		return Challenge{}, fmt.Errorf("%s auth epoch is in the future: %d", Scheme, challenge.epoch)
	}
	if curFilEpoch-challenge.epoch > SigGraceEpochs {
		return Challenge{}, fmt.Errorf("%s auth epoch is too far in the past: %d", Scheme, challenge.epoch)
	}

	arg, err := base64.StdEncoding.DecodeString(hdr.arg)
	if err != nil {
		return Challenge{}, fmt.Errorf("unable to decode optional argument: %s", err.Error())
	}
	sig, err := base64.StdEncoding.DecodeString(hdr.sigB64)
	if err != nil {
		return Challenge{}, fmt.Errorf("unexpected %s auth signature encoding '%s'", Scheme, hdr.sigB64)
	}
	challenge.args = args.New(arg, sig)

	return challenge, nil
}

func Format(c Challenge) string {
	str := fmt.Sprintf(
		"%s %d;%s;%s",
		Scheme,
		c.Epoch(),
		c.Addr().String(),
		base64.StdEncoding.EncodeToString(c.Args().Signature()),
	)
	if len(c.Args().Raw()) != 0 {
		str += ";" + base64.StdEncoding.EncodeToString(c.Args().Raw())
	}
	return str
}

// Verify verifies the signature over the provided challenge args for the given
// address and drand beacon data. The addr should be the storage provider
// worker address (a BLS key) and the entropy must be the drand randomness
// beacon data corresponding to the epoch used during signing.
func Verify(addr filaddr.Address, entropy []byte, challenge Challenge) error {
	var vsr verifySigResult
	if maybeResult, known := challengeCache.Get(challenge.String()); known {
		vsr = maybeResult
	} else {
		err := args.Verify(signature.BLSVerifier{Addr: addr}, entropy, challenge.Args())
		vsr.invalidSigErr = err
		challengeCache.Add(challenge.String(), vsr)
	}
	return vsr.invalidSigErr
}
