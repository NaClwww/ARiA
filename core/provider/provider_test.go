package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{RetryableError{Err: errors.New("x")}, true},
		{&RetryableError{Err: errors.New("x")}, true}, // 指针形态也要命中
		{fmt.Errorf("wrap: %w", &RetryableError{Err: errors.New("x")}), true},
		{errors.New("plain"), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Fatalf("case %d: IsRetryable(%v) = %v, want %v", i, c.err, got, c.want)
		}
	}
}
