package light

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	cmtmath "github.com/cometbft/cometbft/v2/libs/math"
	"github.com/cometbft/cometbft/v2/types"
)

// DefaultTrustLevel - new header can be trusted if at least one correct
// validator signed it.
var DefaultTrustLevel = cmtmath.Fraction{Numerator: 1, Denominator: 3}

// VerifyNonAdjacent verifies non-adjacent untrustedHeader against
// trustedHeader. It ensures that:
//
//		a) trustedHeader can still be trusted (if not, ErrOldHeaderExpired is returned)
//		b) untrustedHeader is valid (if not, ErrInvalidHeader is returned)
//		c) trustLevel ([1/3, 1]) of trustedHeaderVals (or trustedHeaderNextVals)
//	 signed correctly (if not, ErrNewValSetCantBeTrusted is returned)
//		d) more than 2/3 of untrustedVals have signed h2
//	   (otherwise, ErrInvalidHeader is returned)
//	 e) headers are non-adjacent.
//
// maxClockDrift defines how much untrustedHeader.Time can drift into the
// future.
func VerifyNonAdjacent(
	trustedHeader *types.SignedHeader, // height=X
	trustedVals *types.ValidatorSet, // height=X or height=X+1
	untrustedHeader *types.SignedHeader, // height=Y
	untrustedVals *types.ValidatorSet, // height=Y
	trustingPeriod time.Duration,
	now time.Time,
	maxClockDrift time.Duration,
	trustLevel cmtmath.Fraction,
) error {
	if untrustedHeader.Height == trustedHeader.Height+1 {
		return ErrHeaderHeightAdjacent
	}

	if HeaderExpired(trustedHeader, trustingPeriod, now) {
		return ErrOldHeaderExpired{trustedHeader.Time.Add(trustingPeriod), now}
	}

	if err := verifyNewHeaderAndVals(
		untrustedHeader, untrustedVals,
		trustedHeader,
		now, maxClockDrift); err != nil {
		return ErrInvalidHeader{err}
	}

	verifiedSignatureCache := types.NewSignatureCache()
	// Ensure that +`trustLevel` (default 1/3) or more of last trusted validators signed correctly.
	err := trustedVals.VerifyCommitLightTrustingWithCache(trustedHeader.ChainID, untrustedHeader.Commit, trustLevel, verifiedSignatureCache)
	if err != nil {
		switch e := err.(type) {
		case types.ErrNotEnoughVotingPowerSigned:
			return ErrNewValSetCantBeTrusted{e}
		default:
			return e
		}
	}

	// Ensure that +2/3 of new validators signed correctly.
	//
	// NOTE: this should always be the last check because untrustedVals can be
	// intentionally made very large to DOS the light client. not the case for
	// VerifyAdjacent, where validator set is known in advance.
	if err := untrustedVals.VerifyCommitLightWithCache(trustedHeader.ChainID, untrustedHeader.Commit.BlockID,
		untrustedHeader.Height, untrustedHeader.Commit, verifiedSignatureCache); err != nil {
		return ErrInvalidHeader{err}
	}

	return nil
}

// VerifyAdjacent verifies directly adjacent untrustedHeader against
// trustedHeader. It ensures that:
//
//	a) trustedHeader can still be trusted (if not, ErrOldHeaderExpired is returned)
//	b) untrustedHeader is valid (if not, ErrInvalidHeader is returned)
//	c) untrustedHeader.ValidatorsHash equals trustedHeader.NextValidatorsHash
//	d) more than 2/3 of new validators (untrustedVals) have signed h2
//	  (otherwise, ErrInvalidHeader is returned)
//	e) headers are adjacent.
//
// maxClockDrift defines how much untrustedHeader.Time can drift into the
// future.
func VerifyAdjacent(
	trustedHeader *types.SignedHeader, // height=X
	untrustedHeader *types.SignedHeader, // height=X+1
	untrustedVals *types.ValidatorSet, // height=X+1
	trustingPeriod time.Duration,
	now time.Time,
	maxClockDrift time.Duration,
) error {
	if untrustedHeader.Height != trustedHeader.Height+1 {
		return ErrHeaderHeightNotAdjacent
	}

	if HeaderExpired(trustedHeader, trustingPeriod, now) {
		return ErrOldHeaderExpired{trustedHeader.Time.Add(trustingPeriod), now}
	}

	if err := verifyNewHeaderAndVals(
		untrustedHeader, untrustedVals,
		trustedHeader,
		now, maxClockDrift); err != nil {
		return ErrInvalidHeader{err}
	}

	// Check the validator hashes are the same
	if !bytes.Equal(untrustedHeader.ValidatorsHash, trustedHeader.NextValidatorsHash) {
		return ErrValidatorHashMismatch{TrustedHash: trustedHeader.NextValidatorsHash, ValidatorHash: untrustedHeader.ValidatorsHash}
	}

	// Ensure that +2/3 of new validators signed correctly.
	if err := untrustedVals.VerifyCommitLight(trustedHeader.ChainID, untrustedHeader.Commit.BlockID,
		untrustedHeader.Height, untrustedHeader.Commit); err != nil {
		return ErrInvalidHeader{err}
	}

	return nil
}

// Verify combines both VerifyAdjacent and VerifyNonAdjacent functions.
func Verify(
	trustedHeader *types.SignedHeader, // height=X
	trustedVals *types.ValidatorSet, // height=X or height=X+1
	untrustedHeader *types.SignedHeader, // height=Y
	untrustedVals *types.ValidatorSet, // height=Y
	trustingPeriod time.Duration,
	now time.Time,
	maxClockDrift time.Duration,
	trustLevel cmtmath.Fraction,
) error {
	if untrustedHeader.Height != trustedHeader.Height+1 {
		return VerifyNonAdjacent(trustedHeader, trustedVals, untrustedHeader, untrustedVals,
			trustingPeriod, now, maxClockDrift, trustLevel)
	}

	return VerifyAdjacent(trustedHeader, untrustedHeader, untrustedVals, trustingPeriod, now, maxClockDrift)
}

func verifyNewHeaderAndVals(
	untrustedHeader *types.SignedHeader,
	untrustedVals *types.ValidatorSet,
	trustedHeader *types.SignedHeader,
	now time.Time,
	maxClockDrift time.Duration,
) error {
	if err := untrustedHeader.ValidateBasic(trustedHeader.ChainID); err != nil {
		return ErrHeaderValidateBasic{Err: err}
	}

	if untrustedHeader.Height <= trustedHeader.Height {
		return ErrHeaderHeightNotMonotonic{GotHeight: untrustedHeader.Height, OldHeight: trustedHeader.Height}
	}

	if !untrustedHeader.Time.After(trustedHeader.Time) {
		return ErrHeaderTimeNotMonotonic{GotTime: untrustedHeader.Time, OldTime: trustedHeader.Time}
	}

	if !untrustedHeader.Time.Before(now.Add(maxClockDrift)) {
		return ErrHeaderTimeExceedMaxClockDrift{Ti: untrustedHeader.Time, Now: now, Drift: maxClockDrift}
	}

	if !bytes.Equal(untrustedHeader.ValidatorsHash, untrustedVals.Hash()) {
		return ErrValidatorsMismatch{HeaderHash: untrustedHeader.ValidatorsHash, ValidatorsHash: untrustedVals.Hash(), Height: untrustedHeader.Height}
	}

	return nil
}

// ValidateTrustLevel checks that trustLevel is within the allowed range [1/3,
// 1]. If not, it returns an error. 1/3 is the minimum amount of trust needed
// which does not break the security model.
func ValidateTrustLevel(lvl cmtmath.Fraction) error {
	if lvl.Numerator*3 < lvl.Denominator || // < 1/3
		lvl.Numerator > lvl.Denominator || // > 1
		lvl.Denominator == 0 {
		return ErrInvalidTrustLevel{Level: lvl}
	}
	return nil
}

// HeaderExpired return true if the given header expired.
func HeaderExpired(h *types.SignedHeader, trustingPeriod time.Duration, now time.Time) bool {
	expirationTime := h.Time.Add(trustingPeriod)
	return !expirationTime.After(now)
}

// VerifyBackwards verifies an untrusted header with a height one less than
// that of an adjacent trusted header. It ensures that:
//
//		a) untrusted header is valid
//	 b) untrusted header has a time before the trusted header
//	 c) that the LastBlockID hash of the trusted header is the same as the hash
//	 of the trusted header
//
//	 For any of these cases ErrInvalidHeader is returned.
func VerifyBackwards(untrustedHeader, trustedHeader *types.Header) error {
	if err := untrustedHeader.ValidateBasic(); err != nil {
		return ErrInvalidHeader{err}
	}

	if untrustedHeader.ChainID != trustedHeader.ChainID {
		return ErrInvalidHeader{errors.New("header belongs to another chain")}
	}

	if !untrustedHeader.Time.Before(trustedHeader.Time) {
		return ErrInvalidHeader{
			fmt.Errorf("expected older header time %v to be before new header time %v",
				untrustedHeader.Time,
				trustedHeader.Time),
		}
	}

	if !bytes.Equal(untrustedHeader.Hash(), trustedHeader.LastBlockID.Hash) {
		return ErrInvalidHeader{
			fmt.Errorf("older header hash %X does not match trusted header's last block %X",
				untrustedHeader.Hash(),
				trustedHeader.LastBlockID.Hash),
		}
	}

	return nil
}
