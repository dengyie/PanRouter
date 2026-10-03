// Package log 封装 zap 结构化日志。
package log

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New 按级别构建 SugaredLogger;级别非法时回落 info。
func New(level string) (*zap.SugaredLogger, error) {
	lv := zapcore.InfoLevel
	if err := lv.Set(level); err != nil {
		lv = zapcore.InfoLevel
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lv)
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	l, err := cfg.Build(zap.AddCallerSkip(1))
	if err != nil {
		return nil, err
	}
	return l.Sugar(), nil
}
