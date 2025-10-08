package pageparser

import (
	"go.uber.org/zap"
)

type ParserBasic struct {
	Logger *zap.SugaredLogger
}

func NewParserRepo(logger *zap.SugaredLogger) *ParserBasic {
	return &ParserBasic{
		Logger: logger,
	}
}
