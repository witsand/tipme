{
  description = "TipMe — Lightning Network voucher server";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        tipme = pkgs.buildGoModule {
          pname = "tipme";
          version = "0.1.0";
          src = ./.;

          # modernc.org/sqlite is pure Go — no CGO needed.

          # Populated after first `nix build` — see README or run:
          #   nix build 2>&1 | grep "got:"
          vendorHash = "sha256-QJmWaAD29P0C7MUmJ40T4ch9Yc6MSWtzklNE+mic6XM=";

          meta = {
            description = "Printable Lightning Network tip vouchers via LNURL";
            mainProgram = "tipme";
          };
        };
      in
      {
        # `nix build`
        packages.default = tipme;

        # `nix run`
        apps.default = {
          type = "app";
          program = "${tipme}/bin/tipme";
        };

        # `nix develop`
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools        # goimports, godoc, etc.
            golangci-lint
            sqlite-interactive
            rsync
            openssh
          ];

          shellHook = ''
            # ── Load local deploy config ─────────────────────────────────────
            if [ -f .env.deploy ]; then
              set -a
              # shellcheck disable=SC1091
              source .env.deploy
              set +a
            fi

            # Defaults (override in .env.deploy)
            export TIPME_HOST=''${TIPME_HOST:-"user@yourserver.example.com"}
            export TIPME_REMOTE_DIR=''${TIPME_REMOTE_DIR:-"/opt/tipme"}
            export TIPME_SERVICE=''${TIPME_SERVICE:-"tipme"}

            # ── Helper commands ───────────────────────────────────────────────

            build() {
              echo "▶ go build..."
              go build -o ./tipme . && echo "✓ ./tipme ready"
            }

            # Cross-compile for Linux amd64 and push to the server.
            deploy() {
              if [ "$TIPME_HOST" = "user@yourserver.example.com" ]; then
                echo "✗ Set TIPME_HOST in .env.deploy first."
                return 1
              fi

              echo "▶ Building for linux/amd64..."
              GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
                go build -ldflags="-s -w" -o ./tipme-deploy . || return 1

              echo "▶ Syncing binary → $TIPME_HOST:$TIPME_REMOTE_DIR/tipme"
              rsync -az --progress ./tipme-deploy \
                "$TIPME_HOST:$TIPME_REMOTE_DIR/tipme" || return 1

              echo "▶ Syncing static/ → $TIPME_HOST:$TIPME_REMOTE_DIR/static/"
              rsync -az --delete --progress ./static/ \
                "$TIPME_HOST:$TIPME_REMOTE_DIR/static/" || return 1

              echo "▶ Setting permissions and restarting $TIPME_SERVICE..."
              ssh "$TIPME_HOST" "
                chmod +x $TIPME_REMOTE_DIR/tipme
                sudo systemctl restart $TIPME_SERVICE
                sudo systemctl status $TIPME_SERVICE --no-pager -l
              " || return 1

              rm -f ./tipme-deploy
              echo "✓ Deployed!"
            }

            # Quick log tail from the server.
            logs() {
              ssh "$TIPME_HOST" "sudo journalctl -u $TIPME_SERVICE -n 50 --no-pager -f"
            }

            echo ""
            echo "  ⚡ TipMe dev shell"
            echo "  ──────────────────────────────────────────"
            echo "  build    compile binary locally"
            echo "  deploy   cross-compile + rsync + restart"
            echo "  logs     tail service logs on server"
            echo "  ──────────────────────────────────────────"
            echo "  Server:  $TIPME_HOST  ($TIPME_REMOTE_DIR)"
            echo ""
          '';
        };
      });
}
