package models

type User struct {
	Name string
}

type Reader interface {
	Get(id string) string
}
