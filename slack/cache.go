package slack

import (
	"encoding/json"
	"gopkg.in/djherbis/times.v1"
	"io/ioutil"
	"os"
	"strings"
	"time"
)

const cacheDir = "./.terraform/plugins/.cache/terraform-provider-slack"

func saveCacheAsJson(name string, v interface{}) {
	_ = os.MkdirAll(cacheDir, 0755)
	cacheFile := strings.Join([]string{cacheDir, name}, string(os.PathSeparator))

	cache, err := json.Marshal(v)
	if err != nil {
		return // ignore err
	}

	// Write somewhere else and rename into place. A plain write is not atomic,
	// so a reader that arrives mid-write sees a truncated file; rename is, and
	// it costs nothing here because the temp file is in the same directory.
	tmp, err := ioutil.TempFile(cacheDir, name)
	if err != nil {
		return // ignore err
	}

	if _, err := tmp.Write(cache); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return // ignore err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return // ignore err
	}
	_ = os.Chmod(tmp.Name(), 0644)

	if err := os.Rename(tmp.Name(), cacheFile); err != nil {
		_ = os.Remove(tmp.Name())
	} // ignore err
}

func restoreJsonCache(name string, v interface{}) bool {
	_ = os.MkdirAll(cacheDir, 0755)
	cacheFile := strings.Join([]string{cacheDir, name}, string(os.PathSeparator))

	// cache active duration is 6 sec (10 req / min)
	if t, err := times.Stat(cacheFile); err == nil {
		if !time.Now().After(t.ModTime().Add(6 * time.Second)) {
			if bytes, err := ioutil.ReadFile(cacheFile); err == nil {
				return json.Unmarshal(bytes, v) == nil
			}
		}
	}

	return false
}
