package lending

import "errors"

var (
	ErrPoolNotFound       = errors.New("lending: pool not found")
	ErrPoolNotActive      = errors.New("lending: pool not active")
	ErrInsufficientLiquidity = errors.New("lending: insufficient liquidity")
	ErrInsufficientCollateral = errors.New("lending: insufficient collateral")
	ErrBelowMinAmount     = errors.New("lending: below minimum amount")
	ErrOrderNotFound      = errors.New("lending: order not found")
	ErrNotOwner           = errors.New("lending: not owner")
	ErrAlreadyRepaid      = errors.New("lending: already repaid")
	ErrInvalidParam       = errors.New("lending: invalid param")
)
