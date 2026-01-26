package spid

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"code.riba.cloud/go/toolbox-interplanetary/fil"
	filaddr "github.com/filecoin-project/go-address"
	filabi "github.com/filecoin-project/go-state-types/abi"
	filprovider "github.com/filecoin-project/go-state-types/builtin/v9/miner"
	lru "github.com/hashicorp/golang-lru/v2"
	blsgo "github.com/jsign/go-filsigner/bls"
)

const (
	Scheme         = `FIL-SPID-V0`
	sigGraceEpochs = 5 // 30 secs per epoch
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
	challengeCache, _ = lru.New[RawHeader, verifySigResult](sigGraceEpochs * 128)
	beaconCache, _    = lru.New[int64, *fil.LotusBeaconEntry](sigGraceEpochs * 4)
)

type verifySigResult struct {
	invalidSigErr error
}

type RawHeader struct {
	epoch  string
	addr   string
	sigB64 string
	arg    string
}

// Challenge is a parsed SPID authorization that can be verified.
type Challenge struct {
	authHdr   string          // original auth header string
	hdr       RawHeader       // parsed raw header fields
	epoch     int64           // Filecoin epoch
	addr      filaddr.Address // storage provider address
	arg       []byte          // decoded arg as bytes (for signature verification)
	signedArg url.Values      // decoded signed args
	sig       []byte          // decoded signature bytes
}

func (c Challenge) Addr() filaddr.Address {
	return c.addr
}

func (c Challenge) Epoch() int64 {
	return c.epoch
}

func (c Challenge) SignedArg() url.Values {
	return c.signedArg
}

func (c Challenge) String() string {
	return c.authHdr
}

func Parse(s string) (Challenge, error) {
	var challenge Challenge
	challenge.authHdr = s

	res := spAuthRe.FindStringSubmatch(challenge.authHdr)

	if len(res) != 5 {
		return Challenge{}, fmt.Errorf("invalid/unexpected %s Authorization header: %q", Scheme, challenge.authHdr)
	}

	challenge.hdr.epoch, challenge.hdr.addr, challenge.hdr.sigB64, challenge.hdr.arg = res[1], res[2], res[3], res[4]

	var err error
	challenge.addr, err = filaddr.NewFromString(challenge.hdr.addr)
	if err != nil {
		return Challenge{}, fmt.Errorf("unexpected %s auth address:%q", Scheme, challenge.hdr.addr)
	}

	challenge.epoch, err = strconv.ParseInt(challenge.hdr.epoch, 10, 32)
	if err != nil {
		return Challenge{}, fmt.Errorf("unexpected %s auth epoch: %s", Scheme, challenge.hdr.epoch)
	}

	curFilEpoch := int64(fil.ClockMainnet.TimeToEpoch(time.Now()))
	if curFilEpoch < challenge.epoch {
		return Challenge{}, fmt.Errorf("%s auth epoch is in the future: %d", Scheme, challenge.epoch)
	}
	if curFilEpoch-challenge.epoch > sigGraceEpochs {
		return Challenge{}, fmt.Errorf("%s auth epoch is too far in the past: %d", Scheme, challenge.epoch)
	}

	challenge.arg, err = base64.StdEncoding.DecodeString(challenge.hdr.arg)
	if err != nil {
		return Challenge{}, fmt.Errorf("unable to decode optional argument: %s", err.Error())
	}

	challenge.signedArg, err = url.ParseQuery(string(challenge.arg))
	if err != nil {
		return Challenge{}, fmt.Errorf("unable to parse optional argument: %s", err.Error())
	}

	challenge.sig, err = base64.StdEncoding.DecodeString(challenge.hdr.sigB64)
	if err != nil {
		return Challenge{}, fmt.Errorf("unexpected %s auth signature encoding '%s'", Scheme, challenge.hdr.sigB64)
	}

	return challenge, nil
}

func Verify(ctx context.Context, filClient VerificationFilecoinClient, challenge Challenge) error {
	var vsr verifySigResult
	if maybeResult, known := challengeCache.Get(challenge.hdr); known {
		vsr = maybeResult
	} else {
		err := verifySig(ctx, filClient, challenge)
		if err != nil {
			return err
		}
		challengeCache.Add(challenge.hdr, vsr)
	}
	return nil
}

func verifySig(ctx context.Context, filClient VerificationFilecoinClient, challenge Challenge) error {
	be, didFind := beaconCache.Get(challenge.epoch)
	if !didFind {
		var curChallengeTs *fil.LotusTS
		var err error

		// Do it a few times because lotus is getting slower and slower to finalize 😭
		// Can't sleep too much though not to timeout the call
		// spid.bash has been adjusted with a backoff to deal with this as well
		for i := 0; i < 3; i++ {
			curChallengeTs, err = filClient.ChainGetTipSetByHeight(ctx, filabi.ChainEpoch(challenge.epoch), fil.LotusTSK{})
			if err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}

		if err != nil {
			// do not make slow-chain a 500
			return fmt.Errorf(
				"%s signature validation failed for auth header %q: unable to get tipset at height %d (%s): %s",
				Scheme, challenge.authHdr,
				challenge.epoch, fil.ClockMainnet.EpochToTime(filabi.ChainEpoch(challenge.epoch)),
				err,
			)
		}
		bev := curChallengeTs.Blocks()[0].BeaconEntries[len(curChallengeTs.Blocks()[0].BeaconEntries)-1]
		be = &bev

		beaconCache.Add(challenge.epoch, be)
	}

	miFinTs, err := filClient.ChainGetTipSetByHeight(ctx, filabi.ChainEpoch(challenge.epoch)-filprovider.ChainFinality, fil.LotusTSK{})
	if err != nil {
		return err
	}
	mi, err := filClient.StateMinerInfo(ctx, challenge.addr, miFinTs.Key())
	if err != nil {
		return err
	}
	workerAddr, err := filClient.StateAccountKey(ctx, mi.Worker, miFinTs.Key())
	if err != nil {
		return err
	}

	// worker keys are always BLS
	sigMatch, err := blsgo.Verify(
		workerAddr.Payload(),
		append(append([]byte{0x20, 0x20, 0x20}, be.Data...), challenge.arg...),
		challenge.sig,
	)
	if err != nil {
		return err
	}

	if !sigMatch {
		return fmt.Errorf("%s signature validation failed for auth header: %q", Scheme, challenge.authHdr)
	}
	return nil
}
