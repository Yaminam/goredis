package command

import "os"

func processID() int { return os.Getpid() }
