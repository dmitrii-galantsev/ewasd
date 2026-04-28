{
  description = "ewasd - Symlink curated editor/IDE config files into active repositories";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      pkgsFor = system: nixpkgs.legacyPackages.${system};

      mkEwasd = pkgs: pkgs.python3Packages.buildPythonApplication {
        pname = "ewasd";
        version = "0.7.0";
        pyproject = true;

        src = ./.;

        build-system = [ pkgs.python3Packages.setuptools ];

        dependencies = with pkgs.python3Packages; [
          tomlkit
          termcolor
        ];

        nativeCheckInputs = [ pkgs.python3Packages.pytestCheckHook ];

        meta = {
          description = "Symlink curated editor/IDE config files into active repositories";
          homepage = "https://github.com/dmitrii-galantsev/ewasd";
          license = pkgs.lib.licenses.mit;
          mainProgram = "ewasd";
        };
      };
    in
    {
      packages = forAllSystems (system: rec {
        ewasd = mkEwasd (pkgsFor system);
        default = ewasd;
      });

      devShells = forAllSystems (system:
        let pkgs = pkgsFor system; in {
          default = pkgs.mkShell {
            packages = [
              (pkgs.python3.withPackages (ps: with ps; [
                tomlkit termcolor pytest ruff mypy
              ]))
            ];
          };
        }
      );

      overlays.default = final: prev: {
        ewasd = mkEwasd final;
      };
    };
}
