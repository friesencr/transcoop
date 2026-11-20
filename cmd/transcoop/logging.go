package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

func setupLogging(path, command string) (func(), error) {
	previous, prefix, flags := log.Writer(), log.Prefix(), log.Flags()
	var file *os.File
	if path != "-" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = absolute
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("create log directory: %w", err)
		}
		file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, fmt.Errorf("open service log: %w", err)
		}
		log.SetOutput(io.MultiWriter(previous, file))
	}
	log.SetPrefix(fmt.Sprintf("%s[%d] ", command, os.Getpid()))
	log.SetFlags(log.LstdFlags)
	if file != nil {
		log.Printf("service log: %s", path)
	}
	return func() {
		log.SetOutput(previous)
		log.SetPrefix(prefix)
		log.SetFlags(flags)
		if file != nil {
			_ = file.Close()
		}
	}, nil
}
