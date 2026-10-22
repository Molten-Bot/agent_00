package customlibrary

import "testing"

func TestValidateSource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		src     Source
		want    Source
		wantErr bool
	}{
		{
			name: "trims and lowercases name",
			src:  Source{Name: "  Acme-Docs  ", Repo: " git@github.com:acme/docs.git "},
			want: Source{Name: "acme-docs", Repo: "git@github.com:acme/docs.git"},
		},
		{
			name:    "empty name",
			src:     Source{Name: "", Repo: "git@github.com:acme/docs.git"},
			wantErr: true,
		},
		{
			name: "name with uppercase is lowercased",
			src:  Source{Name: "Acme", Repo: "git@github.com:acme/docs.git"},
			want: Source{Name: "acme", Repo: "git@github.com:acme/docs.git"},
		},
		{
			name:    "name with underscore rejected",
			src:     Source{Name: "acme_docs", Repo: "git@github.com:acme/docs.git"},
			wantErr: true,
		},
		{
			name:    "empty repo",
			src:     Source{Name: "acme", Repo: ""},
			wantErr: true,
		},
		{
			name:    "repo with whitespace rejected",
			src:     Source{Name: "acme", Repo: "git@github.com:acme/docs.git --upload-pack=evil"},
			wantErr: true,
		},
		{
			name:    "repo starting with dash rejected",
			src:     Source{Name: "acme", Repo: "--upload-pack=evil"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateSource(tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateSource(%+v) = %+v, nil; want error", tc.src, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateSource(%+v) unexpected error: %v", tc.src, err)
			}
			if got != tc.want {
				t.Fatalf("ValidateSource(%+v) = %+v, want %+v", tc.src, got, tc.want)
			}
		})
	}
}
