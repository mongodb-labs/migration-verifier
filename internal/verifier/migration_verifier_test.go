package verifier

import (
	"testing"
)

func TestExample(t *testing.T) {
	var a int = 0
	if a != 0 {
		t.Errorf("a should be 0")
	}
}

func TestExample2(t *testing.T) {
	var a int = 1
	if a != 1 {
		t.Errorf("a should be 1")
	}
}
