package managedplugin

import (
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

var unixSocketDir = os.TempDir()

// https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-go/22892986#22892986

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randSeq(n int) string {
	b := make([]rune, n)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := range b {
		b[i] = letters[r.Intn(len(letters))]
	}
	return string(b)
}

func GenerateRandomUnixSocketName() string {
	return filepath.Join(unixSocketDir, "loki-"+randSeq(16)+".sock")
}
