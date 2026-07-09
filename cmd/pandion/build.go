// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// buildSpec is the toolchain `pandion build` auto-detects for a project directory:
// the build command to run on the node, any extra apt packages it needs, and
// whether the built-in C++ toolchain is required (kept for C/C++/CMake/Meson,
// skipped otherwise so the node comes up faster).
type buildSpec struct {
	label     string // human name of the detected toolchain (for the banner)
	build     string // build command run on the node after sync
	packages  string // comma-separated extra apt packages ("" = none beyond the toolchain)
	toolchain bool   // keep the built-in C++ toolchain (build-essential/cmake/…)
}

// detectBuild inspects dir for a recognizable project and returns how to build it.
// The order is most-specific-first so that, e.g., a CMake project with a convenience
// Makefile is treated as CMake. Returns ok=false when nothing is recognized.
func detectBuild(dir string) (buildSpec, bool) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	switch {
	case has("CMakeLists.txt"):
		return buildSpec{"CMake (C++)", "cmake -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build -j", "", true}, true
	case has("meson.build"):
		return buildSpec{"Meson (C++)", "meson setup build && meson compile -C build", "meson,ninja-build", true}, true
	case has("Cargo.toml"):
		return buildSpec{"Rust (cargo)", "cargo build --release", "cargo", false}, true
	case has("go.mod"):
		return buildSpec{"Go", "go build ./...", "golang-go", false}, true
	case has("package.json"):
		return buildSpec{"Node.js", "npm ci --no-audit --no-fund && npm run build --if-present", "nodejs,npm", false}, true
	case has("pyproject.toml"):
		return buildSpec{"Python (pyproject)", "pip3 install --break-system-packages .", "python3-pip", false}, true
	case has("requirements.txt"):
		return buildSpec{"Python (requirements)", "pip3 install --break-system-packages -r requirements.txt", "python3-pip", false}, true
	case has("Makefile") || has("makefile"):
		return buildSpec{"Make (C/C++)", "make -j", "", true}, true
	}
	return buildSpec{}, false
}

// runBuild is the one-liner "upload this project and build it in the cloud" flow.
// It auto-detects the toolchain for [dir] (default ".") and delegates to `up`,
// forwarding any extra up-flags and an optional `-- <run cmd>`. With no run
// command it deploys + builds only (like --no-run) so you can `pandion start`,
// `ssh`, or `debug` afterwards.
//
//	pandion build [dir] [up-flags…] [-- <run cmd>]
func runBuild(args []string) {
	flagArgs, runCmd := splitRunCmd(args)

	// an optional leading positional is the project dir; everything else is
	// forwarded verbatim to `up` (so --provider/--size/--id/… all work).
	dir := "."
	rest := flagArgs
	if len(flagArgs) > 0 && !strings.HasPrefix(flagArgs[0], "-") {
		dir = flagArgs[0]
		rest = flagArgs[1:]
	}

	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "build: %q is not a directory\n", dir)
		os.Exit(2)
	}

	spec, ok := detectBuild(dir)
	if !ok {
		fmt.Fprintf(os.Stderr, "build: couldn't detect a toolchain in %q.\n", dir)
		fmt.Fprintln(os.Stderr, "  looked for: CMakeLists.txt, meson.build, Cargo.toml, go.mod, package.json,")
		fmt.Fprintln(os.Stderr, "              pyproject.toml, requirements.txt, Makefile.")
		fmt.Fprintln(os.Stderr, "  build it explicitly instead, e.g.:")
		fmt.Fprintf(os.Stderr, "    pandion up --workspace %s --build '<your build cmd>' -- ./your-binary\n", dir)
		os.Exit(2)
	}

	// caller flags win: only inject a knob we detected if the user didn't set it.
	provided := map[string]bool{}
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			provided[name] = true
		}
	}

	up := append([]string{}, rest...)
	up = append(up, "--workspace", dir)
	if !provided["build"] {
		up = append(up, "--build", spec.build)
	}
	if spec.packages != "" && !provided["packages"] {
		up = append(up, "--packages", spec.packages)
	}
	if !spec.toolchain && !provided["no-toolchain"] {
		up = append(up, "--no-toolchain")
	}

	fmt.Printf("build: detected %s in %s\n", spec.label, dir)
	if runCmd != "" {
		up = append(up, "--", runCmd)
	} else if !provided["no-run"] {
		up = append(up, "--no-run") // build only; start/ssh/debug afterwards
	}
	runUp(up)

	if runCmd == "" {
		fmt.Println("built (no run command given). Next: `pandion start --id <id>`, `pandion ssh --id <id>`, or `pandion debug`.")
	}
}
