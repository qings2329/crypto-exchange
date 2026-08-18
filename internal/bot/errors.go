package bot

import "errors"

var (
	ErrStrategyNotFound = errors.New("bot: strategy not found")
	ErrInvalidParam     = errors.New("bot: invalid param")
	ErrNotOwner         = errors.New("bot: not owner")
	ErrUnauthorized     = errors.New("bot: unauthorized")
)
