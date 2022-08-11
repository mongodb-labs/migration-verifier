package main

import (
	"os"

	"github.com/10gen/migration-verifier/internal/verifier"
)

func main() {
	v := verifier.NewVerifier()
	v.Run(os.Args)
}
