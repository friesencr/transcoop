package remotefs

// File.Sync flushes journal data; namespace durability requires the native gate.
func syncJournalDirectory(string) error { return nil }
