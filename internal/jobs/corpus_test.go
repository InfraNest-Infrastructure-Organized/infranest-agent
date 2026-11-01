package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The masking corpus InfraNest's receiver is tested against too (testdata/masking-corpus.json, copied from InfraNest
// PR #5875): the agent masks a line before it leaves the server and InfraNest masks it again on arrival, so both must turn
// every input into exactly its expected output, or the two lists of shapes drift apart unseen.
type corpus struct {
	Marker string `json:"marker"`
	Lines  []struct {
		Name     string `json:"name"`
		Input    string `json:"input"`
		Expected string `json:"expected"`
	} `json:"lines"`
	Blocks []struct {
		Name  string   `json:"name"`
		Input []string `json:"input"`
		Kept  []string `json:"kept"`
		Gone  []string `json:"gone"`
	} `json:"blocks"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "masking-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.Marker != Masked || len(c.Lines) == 0 || len(c.Blocks) == 0 {
		t.Fatalf("the corpus is not the one this test expects: marker %q, %d lines, %d blocks", c.Marker, len(c.Lines), len(c.Blocks))
	}

	return c
}

func TestMaskMatchesTheSharedCorpus(t *testing.T) {
	for _, l := range loadCorpus(t).Lines {
		if got := Mask(l.Input); got != l.Expected {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", l.Name, l.Input, got, l.Expected)
		}
	}
}

func TestMaskLinesMatchesTheSharedCorpusKeyBlocks(t *testing.T) {
	for _, b := range loadCorpus(t).Blocks {
		out := MaskLines(b.Input)
		if len(out) != len(b.Input) {
			t.Errorf("%s: %d lines in, %d out", b.Name, len(b.Input), len(out))
		}
		for _, k := range b.Kept {
			if !slices.Contains(out, k) {
				t.Errorf("%s: %q should be kept, got %q", b.Name, k, out)
			}
		}
		for _, g := range b.Gone {
			for _, o := range out {
				if strings.Contains(o, g) {
					t.Errorf("%s: %q should be gone, got %q", b.Name, g, out)
				}
			}
		}
	}
}
