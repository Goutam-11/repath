package service

import (
	"example.com/fixture/models"
	"example.com/fixture/store"
)

type Service struct {
	*store.Store
	Name string
}

func NewService(s *store.Store) *Service {
	return &Service{Store: s}
}

func (s *Service) Run() string {
	user := models.User{Name: "x"}
	var r models.Reader = s
	return r.Get(user.Name)
}
