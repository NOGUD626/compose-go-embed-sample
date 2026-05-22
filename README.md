# compose-go-embed-sample

`docker-compose.yml` と `Dockerfile` を 1 つの Go 実行ファイルに内包し、`docker compose` コマンドを使わずに Docker イメージのビルドからコンテナ起動までを単体 exe で完結させるサンプル。

## これは何か

通常、Docker でコンテナを建てるには `docker-compose.yml` と `Dockerfile` という物理ファイルを配布し、`docker compose up` を実行する。

このサンプルは、その構成ファイルを **Go の実行ファイルに焼き込み**、構成ファイルにも `docker compose` コマンドにも依存せず、単体 exe だけでビルド〜起動を行う。

実証している要素:

| 要素 | 役割 |
|------|------|
| `dockerfile_inline` | `Dockerfile` を別ファイルにせず compose ファイルへ畳み込む |
| `//go:embed` | その compose ファイルを exe に内包する（配布物は exe 単体） |
| [compose-go](https://github.com/compose-spec/compose-go) | 実行時に compose ファイルをパースする（`docker compose` 公式のパーサ） |
| [Docker Engine SDK](https://pkg.go.dev/github.com/docker/docker/client) | パース結果を Docker デーモンへの API 呼び出しに変換する |

`compose-go` は「compose ファイルを読む」だけ、Docker SDK は「Docker デーモンに実行させる」だけ。両者は役割が分かれている。

## 仕組み

```
compose.yaml ──[//go:embed]──▶ 実行ファイルに内包（ビルド時）
                                     │
                             [compose-go] パース（実行時）
                                     ▼
                          types.Project（Go の構造体）
                                     │
                  ┌──────────────────┴──────────────────┐
                  ▼                                     ▼
       build.dockerfile_inline               ports / hostname / restart
                  │                                     │
       メモリ上の tar に変換               container.Config / HostConfig へ変換
                  │                                     │
                  ▼                                     ▼
     [Docker SDK] ImageBuild           [Docker SDK] ContainerCreate / Start
                  │                                     │
                  └──────────────────┬──────────────────┘
                                     ▼
                       Docker デーモン ──▶ コンテナ起動
```

## 必要なもの

- **Go** 1.25 以上（`go.mod` で指定。未満の環境でも `GOTOOLCHAIN` が自動取得する）
- **Docker**（デーモンが起動していること。macOS / Windows は Docker Desktop）

> 実行ファイルは「Docker を動かす」のではなく、起動済みの Docker デーモンへ API で指示するクライアント。デーモン本体は別途必要。

## 使い方

```sh
go run .
```

実行すると次を行う:

1. 内包した `compose.yaml` を compose-go でパース
2. `dockerfile_inline` の内容からイメージ `sandbox-ssh-go:latest` をビルド
3. コンテナ `sandbox-ssh-go` を起動（ポート `2223 → 22`）

起動後の接続:

```sh
ssh ubuntu@localhost -p 2223   # パスワード: ubuntu
```

単一実行ファイルとして配布する場合:

```sh
go build -o ssh-up .                                   # 実行ホスト向け
GOOS=windows GOARCH=amd64 go build -o ssh-up.exe .     # Windows 向け
```

`compose.yaml` は本物の compose ファイルなので、デバッグ時は `docker compose -f compose.yaml up` でそのまま起動して挙動を比較できる。

## プロジェクト構成

```
compose-go-embed-sample/
├── compose.yaml   dockerfile_inline で Dockerfile を畳み込んだ compose ファイル
├── main.go        compose-go でパース → Docker SDK でビルド・起動
├── go.mod
└── go.sum
```

`main.go` の主な関数:

| 関数 | 役割 |
|------|------|
| `parseEmbeddedCompose` | 内包した `compose.yaml` を compose-go でパースする |
| `buildImage` | `dockerfile_inline` をメモリ上の tar に詰め `ImageBuild` でビルドする |
| `startContainer` | compose のサービス定義を Docker SDK の設定へ変換し作成・起動する |
| `toPortMaps` | compose の `ports` を `nat.PortSet` / `nat.PortMap` へ変換する |

## 注意

- **学習用サンプル**。SSH のユーザー / パスワードは `ubuntu` / `ubuntu` の固定値。
- 公開ネットワークやマルチユーザー環境で起動したまま放置しないこと。
- 実行ファイルにベタ書きした文字列はバイナリから復元できる。トークン等の秘密情報は埋め込まないこと。

## 後始末

```sh
docker rm -f sandbox-ssh-go
docker rmi sandbox-ssh-go:latest
```
