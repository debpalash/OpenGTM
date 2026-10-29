package schedboot

import "os"

func osGetenv(k string) string { return os.Getenv(k) }
