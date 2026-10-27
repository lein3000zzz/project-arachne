package llm

import (
	"bufio"
	"io"
	"strings"
)

type sseEvent struct {
	name string
	data string
}

// readSSE calls handle for each event of a server-sent event stream. Events are
// separated by blank lines; data lines of one event are joined with newlines.
func readSSE(body io.Reader, handle func(sseEvent) (stop bool, err error)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var (
		event sseEvent
		data  []string
	)

	dispatch := func() (bool, error) {
		if len(data) == 0 {
			event = sseEvent{}
			return false, nil
		}

		event.data = strings.Join(data, "\n")
		stop, err := handle(event)
		event, data = sseEvent{}, nil

		return stop, err
	}

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			if stop, err := dispatch(); stop || err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	_, err := dispatch()

	return err
}
