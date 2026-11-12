package searchapi

// parseID takes an ID in the shape Postgres prints a uuid, in either case,
// and gives it back as Postgres prints it. A malformed ID is then answered
// like a missing one, never passed on to fail as a cast in the database.
func parseID(raw string) (string, bool) {
	if len(raw) != 36 {
		return "", false
	}

	id := []byte(raw)

	for i, c := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return "", false
			}
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
		case 'A' <= c && c <= 'F':
			id[i] = c + 'a' - 'A'
		default:
			return "", false
		}
	}

	return string(id), true
}

// optionalID is parseID for an ID that may be left out.
func optionalID(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}

	return parseID(raw)
}
