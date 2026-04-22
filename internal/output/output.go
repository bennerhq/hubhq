package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

func JSON(w io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

func Table(w io.Writer, headers []string, rows [][]string) error {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for i, h := range headers {
		if _, err := fmt.Fprintf(w, "%-*s", widths[i], h); err != nil {
			return err
		}
		if i < len(headers)-1 {
			if _, err := fmt.Fprint(w, "  "); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for i, width := range widths {
		if _, err := fmt.Fprint(w, strings.Repeat("-", width)); err != nil {
			return err
		}
		if i < len(widths)-1 {
			if _, err := fmt.Fprint(w, "  "); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, row := range rows {
		for i, cell := range row {
			if _, err := fmt.Fprintf(w, "%-*s", widths[i], cell); err != nil {
				return err
			}
			if i < len(row)-1 {
				if _, err := fmt.Fprint(w, "  "); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}

func SortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
