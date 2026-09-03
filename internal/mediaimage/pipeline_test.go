package mediaimage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/processor"
)

func TestProcessLeavesOutputValidationToManager(t *testing.T) {
	magick := writeExecutable(t, `#!/bin/sh
if [ "$1" = "identify" ]; then
    case "$3" in
        *channels*) exit 1 ;;
    esac
    printf 'PNG|10|12|1|sRGB\n'
    exit 0
fi
last=""
for arg do last="$arg"; done
case "$last" in
    jpg:*) : > "${last#jpg:}" ;;
esac
`)
	root := t.TempDir()
	input := filepath.Join(root, "input.png")
	output := filepath.Join(root, "output.jpg")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline := Pipeline{
		Magick: processor.ToolRunner{Path: magick}, MaxWidth: 100, MaxHeight: 100,
		MaxPixels: 10000, MaxOutputBytes: 1024,
	}
	if _, _, err := pipeline.Process(context.Background(), input, output, domain.DefaultPolicy()); err != nil {
		t.Fatalf("process should not perform a second validation: %v", err)
	}
	if _, err := pipeline.Validate(context.Background(), output); err == nil {
		t.Fatal("manager validation should reject the invalid output")
	}
}

func writeExecutable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
