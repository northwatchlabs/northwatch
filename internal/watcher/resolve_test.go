package watcher

import "testing"

// fakeProbe is a deterministic sourceProbe for table-driven tests.
type fakeProbe struct {
	files     map[string]bool
	envValue  string
	homePath  string
	inCluster bool
}

func (f fakeProbe) fileExists(path string) bool   { return f.files[path] }
func (f fakeProbe) kubeconfigEnv() string         { return f.envValue }
func (f fakeProbe) homeKubeconfigPath() string    { return f.homePath }
func (f fakeProbe) inClusterTokenAvailable() bool { return f.inCluster }

func TestResolveSource(t *testing.T) {
	t.Parallel()

	const flagPath = "/tmp/explicit-kubeconfig"
	const homePath = "/home/dev/.kube/config"

	cases := []struct {
		name       string
		opts       Options
		probe      fakeProbe
		wantSource credentialSource
		wantPath   string
		wantErr    bool
	}{
		{
			name: "flag beats everything",
			opts: Options{KubeconfigPath: flagPath},
			probe: fakeProbe{
				files:     map[string]bool{flagPath: true, homePath: true},
				envValue:  "/some/env/kubeconfig",
				homePath:  homePath,
				inCluster: true,
			},
			wantSource: sourceFlag,
			wantPath:   flagPath,
		},
		{
			name: "in-cluster beats env and home",
			opts: Options{},
			probe: fakeProbe{
				files:     map[string]bool{homePath: true},
				envValue:  "/some/env/kubeconfig",
				homePath:  homePath,
				inCluster: true,
			},
			wantSource: sourceInCluster,
		},
		{
			name: "env beats home when in-cluster absent",
			opts: Options{},
			probe: fakeProbe{
				files:     map[string]bool{homePath: true},
				envValue:  "/some/env/kubeconfig",
				homePath:  homePath,
				inCluster: false,
			},
			wantSource: sourceEnv,
		},
		{
			name: "home wins when nothing else set",
			opts: Options{},
			probe: fakeProbe{
				files:     map[string]bool{homePath: true},
				envValue:  "",
				homePath:  homePath,
				inCluster: false,
			},
			wantSource: sourceHome,
		},
		{
			name:       "nothing available → sourceNone with error",
			opts:       Options{},
			probe:      fakeProbe{files: map[string]bool{}, homePath: homePath},
			wantSource: sourceNone,
			wantErr:    true,
		},
		{
			name: "flag set but file missing → error",
			opts: Options{KubeconfigPath: flagPath},
			probe: fakeProbe{
				files:    map[string]bool{}, // flagPath NOT present
				homePath: homePath,
			},
			wantSource: sourceNone,
			wantErr:    true,
		},
		{
			name: "env multi-path value is still sourceEnv",
			opts: Options{},
			probe: fakeProbe{
				files:    map[string]bool{},
				envValue: "/a:/b:/c",
				homePath: homePath,
			},
			wantSource: sourceEnv,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSource, gotPath, err := resolveSource(tc.opts, tc.probe)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got nil err, want error")
				}
				if gotSource != tc.wantSource {
					t.Errorf("source = %v, want %v", gotSource, tc.wantSource)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if gotSource != tc.wantSource {
				t.Errorf("source = %v, want %v", gotSource, tc.wantSource)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}
