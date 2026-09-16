{
  description = "cnb2api - CNB NPC 接口的 OpenAI 兼容反向代理";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin" # nixpkgs 26.11 起已移除 x86_64-darwin
      ];

      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);

      # 单包定义：供 packages / checks / nixosModules 复用
      mkPackage =
        pkgs:
        pkgs.buildGoModule {
          pname = "cnb2api";
          version = "0.1.0";
          src = self;
          subPackages = [ "cmd/server" ];
          # 项目无第三方依赖，无需 vendor 目录（有依赖时改为 vendorHash = "sha256-..."）
          vendorHash = null;
          ldflags = [
            "-s"
            "-w"
          ];
          env.CGO_ENABLED = 0;
          # 二进制取自 cmd/server，产物名是 server；补一个 cnb2api 名字，避免
          # nix profile install 后只有个含义不明的 server 命令
          postInstall = ''
            ln -s server $out/bin/cnb2api
          '';
          meta = {
            description = "CNB NPC 接口的 OpenAI 兼容反向代理";
            homepage = "https://github.com/Mooling0602/cnb2api";
            license = pkgs.lib.licenses.mit;
            mainProgram = "cnb2api";
            platforms = systems;
          };
        };
    in
    {
      packages = forAllSystems (system: {
        default = mkPackage nixpkgs.legacyPackages.${system};
      });

      # nix run .            → 默认无配置文件，走环境变量（推荐 CNB2API_API_KEY）
      # nix run . -- -config ~/cnb2api.json
      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/server";
          meta.description = "启动 cnb2api 服务";
        };
      });

      checks = forAllSystems (system: {
        # 复用 buildGoModule 的检查阶段跑 go test ./...
        tests = (mkPackage nixpkgs.legacyPackages.${system}).overrideAttrs (old: {
          pname = old.pname + "-tests";
          doCheck = true;
          subPackages = [ ]; # 放开子包过滤，确保 internal/** 的测试也被执行
          installPhase = "touch $out";
        });
      });

      devShells = forAllSystems (system: {
        default = nixpkgs.legacyPackages.${system}.mkShell {
          packages = with nixpkgs.legacyPackages.${system}; [
            go
            gopls
            gotools # goimports / go vet 辅助
            golangci-lint
            nixfmt
          ];
        };
      });

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);

      # NixOS 模块：services.cnb2api.enable = true;
      nixosModules.default =
        {
          config,
          lib,
          pkgs,
          ...
        }:
        let
          cfg = config.services.cnb2api;
          pkg = self.packages.${pkgs.stdenv.hostPlatform.system}.default;

          # systemd 实际执行的 wrapper：把 credential 里的 key 读进环境变量再启动服务
          wrapper = pkgs.writeShellScriptBin "cnb2api-service" ''
            # 注意：这里刻意不用 writeShellApplication —— 它会引入 ShellCheck，
            # 进而拖入整个 GHC/Haskell 工具链，构建代价过大。
            export PATH=${lib.makeBinPath [ pkgs.coreutils ]}:$PATH
            ${lib.optionalString (cfg.apiKeyFile != null) ''
              # LoadCredential 把裸 key 放在 $CREDENTIALS_DIRECTORY/cnb2api_api_key
              if [ -n "''${CREDENTIALS_DIRECTORY:-}" ] && [ -r "$CREDENTIALS_DIRECTORY/cnb2api_api_key" ]; then
                CNB2API_API_KEY="$(cat "$CREDENTIALS_DIRECTORY/cnb2api_api_key")"
                export CNB2API_API_KEY
              fi
            ''}
            exec ${lib.getExe cfg.package} -config ${
              pkgs.writeText "cnb2api-config.json" (
                builtins.toJSON {
                  listen = cfg.listen;
                  api_key = ""; # 实际 key 运行时由环境变量覆盖
                  model = cfg.model;
                  models = cfg.models;
                  upstream = cfg.upstream;
                  pool_min = cfg.poolMin;
                  pool_max = cfg.poolMax;
                  ttl_minutes = cfg.ttlMinutes;
                }
              )
            } "$@"
          '';
        in
        {
          options.services.cnb2api = {
            enable = lib.mkEnableOption "cnb2api OpenAI 兼容反向代理";

            package = lib.mkOption {
              type = lib.types.package;
              default = pkg;
              description = "cnb2api 包。";
            };

            listen = lib.mkOption {
              type = lib.types.str;
              default = ":7863";
              description = "监听地址。";
            };

            apiKeyFile = lib.mkOption {
              type = lib.types.nullOr lib.types.path;
              default = null;
              description = ''
                存放 API 鉴权 key 的**纯文本**文件（内容即 key，不是 KEY=value）；
                通常配合 sops-nix / agenix 使用。
                null 表示不鉴权。

                注意：本模块启用了 ProtectHome，因此 key 文件不能放在 /home 或 /root
                下，否则 systemd 无法读取；请放在 /run/secrets 等位置，或用
                systemd.services.cnb2api.serviceConfig.LoadCredential 自行注入。
                路径来自 Nix store 时会存在于 world-readable 的 store 中（不推荐）。
              '';
            };

            model = lib.mkOption {
              type = lib.types.str;
              default = "deepseek-v4.1-flash";
              description = "默认模型名。";
            };

            models = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [
                "deepseek-v4.1-flash"
              ];
              description = "对外暴露的模型列表。";
            };

            upstream = lib.mkOption {
              type = lib.types.str;
              default = "https://cnb.cool";
              description = "上游基础地址。";
            };

            poolMin = lib.mkOption {
              type = lib.types.ints.positive;
              default = 2;
              description = "凭证池最小数。";
            };

            poolMax = lib.mkOption {
              type = lib.types.ints.positive;
              default = 8;
              description = "凭证池最大数。";
            };

            ttlMinutes = lib.mkOption {
              type = lib.types.ints.positive;
              default = 30;
              description = "凭证 TTL（分钟）。";
            };

            openFirewall = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "是否放行防火墙端口。";
            };

            # 只读：systemd 实际执行的 wrapper 派生（含凭证读取逻辑）
            internal = lib.mkOption {
              type = lib.types.attrs;
              readOnly = true;
              internal = true;
              description = "内部实现细节，供测试/调试引用。";
            };
          };

          config = lib.mkIf cfg.enable {
            services.cnb2api.internal.wrapper = wrapper;

            systemd.services.cnb2api = {
              description = "cnb2api - CNB NPC 的 OpenAI 兼容反向代理";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];
              serviceConfig = {
                ExecStart = lib.getExe wrapper;
                Environment = [
                  "CNB2API_LISTEN=${cfg.listen}"
                  "CNB2API_MODEL=${cfg.model}"
                  "CNB2API_UPSTREAM=${cfg.upstream}"
                  "CNB2API_POOL_MIN=${toString cfg.poolMin}"
                  "CNB2API_POOL_MAX=${toString cfg.poolMax}"
                  "CNB2API_TTL_MINUTES=${toString cfg.ttlMinutes}"
                ];
                # key 以 credential 形式落盘，再由 wrapper 读入环境变量，避免出现在
                # systemd 的 Environment / Nix store 中
                LoadCredential = lib.optional (cfg.apiKeyFile != null) "cnb2api_api_key:${cfg.apiKeyFile}";
                Restart = "on-failure";
                RestartSec = 3;
                DynamicUser = true;
                StateDirectory = "cnb2api";
                WorkingDirectory = "/var/lib/cnb2api";
                NoNewPrivileges = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                PrivateTmp = true;
              };
            };

            # listen 可能是 ":7863" / "0.0.0.0:7863" / "[::]:7863"，统一取最后一段端口
            networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [
              (lib.toInt (lib.last (lib.splitString ":" cfg.listen)))
            ];
          };
        };
    };
}
