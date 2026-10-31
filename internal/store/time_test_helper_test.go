package store

import "time"

func futureTime() time.Time { return time.Now().Add(time.Hour) }
