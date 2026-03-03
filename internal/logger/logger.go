package logger

import (
	"go.uber.org/zap"
)

var Log *zap.Logger

// InitLogger инициализирует глобальный логгер
func InitLogger(environment string) error {
	var err error

	if environment == "production" {
		// Production конфигурация: JSON формат
		Log, err = zap.NewProduction()
	} else {
		// Development конфигурация: человеко-читаемый формат
		Log, err = zap.NewDevelopment()
	}

	if err != nil {
		return err
	}

	// Заменяем глобальный логгер
	zap.ReplaceGlobals(Log)

	return nil
}

// GetLogger возвращает логгер
func GetLogger() *zap.Logger {
	if Log == nil {
		// Fallback на базовый логгер
		Log, _ = zap.NewDevelopment()
	}
	return Log
}

// Sync синхронизирует буфер логгера (вызывать при выходе)
func Sync() {
	if Log != nil {
		_ = Log.Sync()
	}
}
