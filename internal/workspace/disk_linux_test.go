package workspace

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBtrfsDiskPoolIDUsesFilesystemUUID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		uuid [16]byte
		want string
	}{
		{name: "first subvolume", uuid: [16]byte{1}, want: "btrfs:01000000000000000000000000000000"},
		{name: "second subvolume", uuid: [16]byte{1}, want: "btrfs:01000000000000000000000000000000"},
		{name: "different filesystem", uuid: [16]byte{2}, want: "btrfs:02000000000000000000000000000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			got, err := btrfsDiskPoolID(path, func(fd int, info *btrfsFSInfo) error {
				var opened, expected unix.Stat_t
				if err := unix.Fstat(fd, &opened); err != nil {
					t.Fatal(err)
				}
				if err := unix.Stat(path, &expected); err != nil {
					t.Fatal(err)
				}
				if opened.Dev != expected.Dev || opened.Ino != expected.Ino {
					t.Fatal("filesystem UUID query used the wrong path")
				}
				info.fsid = tc.uuid
				return nil
			})
			if err != nil || got != tc.want {
				t.Fatalf("pool ID = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestBtrfsDiskPoolIDReturnsQueryError(t *testing.T) {
	t.Parallel()

	got, err := btrfsDiskPoolID(t.TempDir(), func(int, *btrfsFSInfo) error { return unix.EPERM })
	if got != "" || !errors.Is(err, unix.EPERM) {
		t.Fatalf("pool ID = %q, %v, want permission error", got, err)
	}
}
