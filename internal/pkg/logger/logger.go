package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New 创建一个结构化 logger。
// mode 为 "debug" 输出可读日志，否则输出 JSON 生产日志。
func New(mode string) (*zap.Logger, error) {
	var cfg zap.Config
	if mode == "debug" {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		cfg = zap.NewProductionConfig()
	}
	return cfg.Build()
}
