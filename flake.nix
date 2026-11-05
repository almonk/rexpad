{
  description = "Live todo list for coding agents, drawn in a Rex pane";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          # go.mod requires go >= 1.27; the nixpkgs default is still 1.26, so
          # override the builder's go rather than the package's.
          rexpad = (pkgs.buildGoModule.override { go = pkgs.go_1_27; }) {
            pname = "rexpad";
            version = "0.1.0-unstable-${builtins.substring 0 8 self.lastModifiedDate}";
            src = self;
            vendorHash = "sha256-KQLcYktENU7fCsAH0RCp7zV1Y/YWkheRB05BNCA6F9s=";
            meta = {
              description = "Live todo list for coding agents, drawn in a Rex pane";
              homepage = "https://github.com/almonk/rexpad";
              license = pkgs.lib.licenses.mit;
              mainProgram = "rexpad";
            };
          };

          default = self.packages.${system}.rexpad;
        }
      );
    };
}
