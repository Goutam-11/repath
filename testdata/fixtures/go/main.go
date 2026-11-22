package main

import (
	"fmt"

	"example.com/fixture/service"
	"example.com/fixture/store"
)

func main() {
	s := store.New()
	var svc *service.Service = service.NewService(s)
	fmt.Println(svc.Run())
}

func helper() int { return 1 }
