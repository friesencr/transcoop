package farm

// File data is flushed through File.Sync; Windows does not expose portable
// directory fsync through os.File.
func syncDirectory(path string) error { return nil }
