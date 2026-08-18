package staking

import "errors"

var (
	ErrProductNotFound    = errors.New("staking: product not found")
	ErrDelegationNotFound = errors.New("staking: delegation not found")
	ErrInvalidAmount      = errors.New("staking: amount must be positive")
	ErrBelowMinAmount     = errors.New("staking: amount below product minimum")
	ErrUnsupportedAsset   = errors.New("staking: unsupported asset")
	ErrNotOwner           = errors.New("staking: not owner")
	ErrAlreadyUnbonded    = errors.New("staking: already unbonded")
	ErrUnbondPending      = errors.New("staking: unbond not confirmed yet")
	ErrUnsupportedChain   = errors.New("staking: unsupported chain")
)
