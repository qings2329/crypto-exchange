package copytrade

import "errors"

var (
	ErrLeadNotFound     = errors.New("copytrade: lead not found")
	ErrFollowNotFound   = errors.New("copytrade: follow not found")
	ErrInvalidParam     = errors.New("copytrade: invalid param")
	ErrNotOwner         = errors.New("copytrade: not owner")
	ErrAlreadyFollowing = errors.New("copytrade: already following this lead")
	ErrUnsupportedAsset = errors.New("copytrade: unsupported quote asset")
	ErrBelowMinNotional = errors.New("copytrade: follower notional below minimum")
)
