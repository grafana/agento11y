package capturemode

import "testing"

func TestResolve(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want Mode
	}{
		{
			name: "no-local flag beats environment",
			req: Request{
				Flag:            FlagNoLocal,
				EnvValue:        "true",
				EnvKey:          "AGENTO11Y_LOCAL",
				DaemonSupported: true,
			},
			want: Mode{Source: SourceFlag},
		},
		{
			name: "local flag beats environment",
			req: Request{
				Flag:            FlagLocal,
				EnvValue:        "false",
				EnvKey:          "AGENTO11Y_LOCAL",
				HasCloudCreds:   true,
				DaemonSupported: true,
			},
			want: Mode{Local: true, Source: SourceFlag},
		},
		{
			name: "parseable environment value",
			req: Request{
				EnvValue:        "false",
				EnvKey:          "SIGIL_LOCAL",
				DaemonSupported: true,
			},
			want: Mode{Source: SourceEnv, EnvKey: "SIGIL_LOCAL"},
		},
		{
			name: "unsupported platform",
			req: Request{
				DaemonSupported: false,
			},
			want: Mode{Source: SourceUnsupported},
		},
		{
			name: "Cloud credentials",
			req: Request{
				HasCloudCreds:   true,
				DaemonSupported: true,
			},
			want: Mode{Source: SourceCredentials},
		},
		{
			name: "no credentials defaults local",
			req: Request{
				DaemonSupported: true,
			},
			want: Mode{Local: true, Source: SourceDefault},
		},
		{
			name: "unparseable environment value uses default",
			req: Request{
				EnvValue:        "enabled",
				EnvKey:          "AGENTO11Y_LOCAL",
				DaemonSupported: true,
			},
			want: Mode{Local: true, Source: SourceDefault},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Resolve(tt.req); got != tt.want {
				t.Fatalf("Resolve(%+v) = %+v, want %+v", tt.req, got, tt.want)
			}
		})
	}
}
