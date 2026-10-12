package main

import (
	"testing"
	"time"
)

func TestCheckInterval(t *testing.T) {
	for d, ok := range map[time.Duration]bool{
		time.Second: true, 5 * time.Second: true, time.Minute: true,
		100 * time.Millisecond: false, 1500 * time.Millisecond: false, 0: false, -time.Second: false,
	} {
		if err := checkInterval(d); (err == nil) != ok {
			t.Errorf("checkInterval(%s) = %v", d, err)
		}
	}
}
