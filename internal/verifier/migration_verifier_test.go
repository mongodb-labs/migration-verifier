package verifier

import (
	"testing"
)

func TestExample(t *testing.T) {
	var a int = 0
	if a != 0 {
		t.Errorf("a should b 0")
	}
}
