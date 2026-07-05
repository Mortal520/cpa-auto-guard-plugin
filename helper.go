package main

import "time"

// nowTime returns the current time. Centralized so probe/classify logic can be
// unit-tested with a fixed clock in the future by swapping this function.
func nowTime() time.Time {
	return time.Now()
}
