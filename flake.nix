{
  description = "ewasd — explicit, journaled repository overlays";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system:
        f system (import nixpkgs { inherit system; })
      );
    in {
      packages = forAllSystems (_system: pkgs: {
        ewasd = pkgs.buildGoModule {
          pname = "ewasd";
          version = "2.0.0";
          src = ./.;
          vendorHash = null;
          subPackages = [ "cmd/ewasd" ];
          ldflags = [ "-s" "-w" ];
          nativeBuildInputs = [ pkgs.git ];
          doCheck = true;
          checkPhase = ''
            runHook preCheck
            go test ./...
            runHook postCheck
          '';
          meta = {
            description = "Explicit, journaled repository overlay manager";
            homepage = "https://github.com/dmitrii-galantsev/ewasd";
            license = pkgs.lib.licenses.mit;
            mainProgram = "ewasd";
          };
        };
        default = self.packages.${_system}.ewasd;
      });

      apps = forAllSystems (system: _pkgs: {
        default = {
          type = "app";
          program = "${self.packages.${system}.ewasd}/bin/ewasd";
        };
      });

      devShells = forAllSystems (_system: pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.git ];
        };
      });

      overlays.default = final: _prev: {
        ewasd = self.packages.${final.system}.ewasd;
      };
    };
}
