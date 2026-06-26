{
  description = "ksync — local-development sync loop for Kubernetes (watch local kustomize dirs; render/diff/apply with ArgoCD-parity semantics)";

  nixConfig = {
    extra-substituters = [ "https://motoki317-ksync.cachix.org" ];
    extra-trusted-public-keys = [ "motoki317-ksync.cachix.org-1:uDM0RWapTkolNEgkcqQIGpmJc3bumFf+y3RYj50jQA0=" ];
  };

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    git-hooks = {
      url = "github:cachix/git-hooks.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, flake-utils, git-hooks }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        # go.mod pins go 1.26.x; build with the matching toolchain.
        buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_26; };

        version = self.shortRev or self.dirtyShortRev or "dev";

        # nixpkgs' helm 4.2.0 checkPhase is broken at this pin (substitute() on a
        # missing test file); the compile itself succeeds, so skip the tests.
        helm = pkgs.kubernetes-helm.overrideAttrs (_: { doCheck = false; });

        # Git-hook entrypoints. They delegate to the justfile recipes (the single
        # source of truth for the project's commands) and only supply the toolchain
        # on PATH. GOTOOLCHAIN=local pins go to the nix-provided go_1_26 (which
        # satisfies go.mod) so no toolchain is fetched at hook time.
        preCommitHook = pkgs.writeShellApplication {
          name = "ksync-pre-commit";
          runtimeInputs = [ pkgs.go_1_26 pkgs.just ];
          text = ''
            export GOTOOLCHAIN=local
            just pre-commit
          '';
        };
        # The flake build uses the system nix already on PATH; only `just` is supplied.
        nixBuildHook = pkgs.writeShellApplication {
          name = "ksync-nix-build";
          runtimeInputs = [ pkgs.just ];
          text = "just nix-build";
        };

        # Local-only git hooks. Not exposed under `checks`: the build/test hooks need
        # the working tree's go-module cache (and nix-in-nix for the flake build),
        # neither of which exists in the pure `nix flake check` sandbox.
        preCommitCheck = git-hooks.lib.${system}.run {
          src = ./.;
          hooks = {
            # Every commit must compile and pass tests (incl. the leak guard).
            ksync-pre-commit = {
              enable = true;
              name = "ksync build + checks (just build, gofmt, vet, go test incl. leakcheck)";
              entry = pkgs.lib.getExe preCommitHook;
              language = "system";
              pass_filenames = false;
            };
            # Validate the Nix path (a stale vendorHash or a flake error) only when a
            # commit touches a dependency/flake file — the only changes that can break
            # it. Slow (no cross-commit cache), so it stays off the per-commit hot path.
            ksync-nix-build = {
              enable = true;
              name = "nix build .#ksync (only on go.mod/go.sum/flake changes)";
              entry = pkgs.lib.getExe nixBuildHook;
              language = "system";
              pass_filenames = false;
              files = "(^go\\.(mod|sum)$|^flake\\.(nix|lock)$)";
            };
          };
        };
      in
      {
        packages = {
          default = self.packages.${system}.ksync;

          ksync = buildGoModule {
            pname = "ksync";
            inherit version;
            src = ./.;
            # proxyVendor makes the vendor derivation a `go mod download` module
            # cache — a pure function of go.mod/go.sum. The default (`go mod
            # vendor`) also depends on which packages the *source* imports, so a
            # new import of an already-required module silently stales
            # vendorHash without touching go.mod — invisible to the
            # ksync-nix-build hook below, which fires only on dependency/flake
            # files. proxyVendor is what makes that file gate sound.
            proxyVendor = true;
            vendorHash = "sha256-BjCcLbrZnoPtS+hGmLKSgZ5RT0t1S/AYeWoX4MbVk4Q=";
            subPackages = [ "cmd/ksync" ];
            ldflags = [ "-s" "-w" "-X main.version=${version}" ];
            env.CGO_ENABLED = 0;

            meta = {
              description = "Local-development sync loop for Kubernetes";
              homepage = "https://github.com/motoki317/ksync";
              license = pkgs.lib.licenses.mit;
              mainProgram = "ksync";
            };
          };
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.ksync}/bin/ksync";
        };

        devShells.default = pkgs.mkShell {
          inherit (preCommitCheck) shellHook;
          packages = (with pkgs; [
            go_1_26
            gopls
            golangci-lint
            just
            kubectl
            kustomize
          ]) ++ [ helm ] ++ preCommitCheck.enabledPackages;
        };
      });
}
