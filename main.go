// compose-go + dockerfile_inline を使い、docker-compose.yml 相当の設定を
// 1 ファイルへ畳んで Go の実行ファイルに内包するサンプル。
//
// 処理の流れ:
//   ① //go:embed で compose.yaml を exe に焼き込む（物理ファイル不要）
//   ② compose-go が実行時に compose.yaml をパース → types.Project
//   ③ build.dockerfile_inline の中身をメモリ上の tar に詰める
//   ④ Docker SDK でイメージをビルド（cli.ImageBuild）
//   ⑤ Docker SDK でコンテナを作成・起動（ContainerCreate / ContainerStart）
//
// compose-go は「YAML を読む」だけ、Docker SDK(cli) は「デーモンに実行させる」
// だけで、役割が分かれている点に注目。
package main

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/go-connections/nat"
)

// compose.yaml をバイト列としてバイナリへ埋め込む。
// 実行時にディスクからは一切読まない。
//
//go:embed compose.yaml
var composeYAML []byte

// サンプルが対象とするプロジェクト名・サービス名・既定値。
const (
	projectName     = "compose-go-embed-sample"
	serviceName     = "ubuntu-ssh"
	dockerfileName  = "Dockerfile"
	defaultProtocol = "tcp"
	bindAllIPs      = "0.0.0.0"
	shortIDLen      = 12
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "エラー:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// ① + ② 埋め込んだ compose.yaml を compose-go でパース
	project, err := parseEmbeddedCompose(ctx)
	if err != nil {
		return fmt.Errorf("compose のパースに失敗: %w", err)
	}

	svc, ok := project.Services[serviceName]
	if !ok {
		return fmt.Errorf("サービス %q が compose 内に存在しない", serviceName)
	}
	if svc.Build == nil || svc.Build.DockerfileInline == "" {
		return fmt.Errorf("サービス %q に dockerfile_inline がない", serviceName)
	}

	// Docker デーモンへ接続（unix socket / Windows named pipe を自動判別）
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("Docker クライアントの生成に失敗: %w", err)
	}
	defer cli.Close()

	if _, err := cli.Ping(ctx); err != nil {
		return fmt.Errorf("Docker デーモンに接続できない（Docker は起動している?）: %w", err)
	}

	// ③ + ④ dockerfile_inline からイメージをビルド
	fmt.Printf("[1/2] イメージ %s をビルド中...\n", svc.Image)
	if err := buildImage(ctx, cli, svc); err != nil {
		return fmt.Errorf("イメージのビルドに失敗: %w", err)
	}

	// ⑤ コンテナを作成・起動
	fmt.Printf("[2/2] コンテナ %s を起動中...\n", svc.ContainerName)
	id, err := startContainer(ctx, cli, svc)
	if err != nil {
		return fmt.Errorf("コンテナの起動に失敗: %w", err)
	}

	fmt.Println()
	fmt.Printf("起動完了  container=%s\n", shortID(id))
	printAccessInfo(svc)
	return nil
}

// parseEmbeddedCompose は exe に内包した compose.yaml を compose-go でパースする。
// ConfigFile.Content にバイト列を直接渡すため、ディスク上のファイルは読まない。
func parseEmbeddedCompose(ctx context.Context) (*types.Project, error) {
	details := types.ConfigDetails{
		WorkingDir: ".",
		ConfigFiles: []types.ConfigFile{
			{Filename: "compose.yaml", Content: composeYAML},
		},
		Environment: types.Mapping{},
	}
	return loader.LoadWithContext(ctx, details, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
	})
}

// buildImage は build.dockerfile_inline の中身をメモリ上の tar に詰め、
// それをビルドコンテキストとして Docker デーモンへ送る。
func buildImage(ctx context.Context, cli *client.Client, svc types.ServiceConfig) error {
	buildCtx, err := tarFromDockerfile(svc.Build.DockerfileInline)
	if err != nil {
		return err
	}

	resp, err := cli.ImageBuild(ctx, buildCtx, dockertypes.ImageBuildOptions{
		Tags:       []string{svc.Image},
		Dockerfile: dockerfileName,
		BuildArgs:  svc.Build.Args,
		Remove:     true,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// ビルドログ（JSON ストリーム）を整形表示しつつ最後まで読み切る。
	// 読み切らないとビルド完了を待たずに先へ進んでしまう。
	return jsonmessage.DisplayJSONMessagesStream(resp.Body, os.Stdout, 0, false, nil)
}

// tarFromDockerfile は Dockerfile 文字列だけを含む tar をメモリ上に組み立てる。
// 一時ファイルは作らない。
func tarFromDockerfile(dockerfile string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	content := []byte(dockerfile)
	hdr := &tar.Header{Name: dockerfileName, Mode: 0o644, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(content); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// startContainer は compose のサービス定義を Docker SDK の設定へ変換し、
// 同名コンテナを掃除してから作成・起動する。
func startContainer(ctx context.Context, cli *client.Client, svc types.ServiceConfig) (string, error) {
	exposed, bindings, err := toPortMaps(svc.Ports)
	if err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:        svc.Image,
		Hostname:     svc.Hostname,
		ExposedPorts: exposed,
	}
	hostCfg := &container.HostConfig{
		PortBindings:  bindings,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(svc.Restart)},
	}

	// サンプルなので、前回の同名コンテナが残っていれば強制削除してから作り直す。
	_ = cli.ContainerRemove(ctx, svc.ContainerName, container.RemoveOptions{Force: true})

	created, err := cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, svc.ContainerName)
	if err != nil {
		return "", err
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	return created.ID, nil
}

// toPortMaps は compose の ports 定義を Docker SDK の
// ExposedPorts(PortSet) と PortBindings(PortMap) へ変換する。
func toPortMaps(portList []types.ServicePortConfig) (nat.PortSet, nat.PortMap, error) {
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for _, p := range portList {
		protocol := p.Protocol
		if protocol == "" {
			protocol = defaultProtocol
		}
		port, err := nat.NewPort(protocol, fmt.Sprint(p.Target))
		if err != nil {
			return nil, nil, err
		}
		exposed[port] = struct{}{}
		bindings[port] = []nat.PortBinding{{HostIP: bindAllIPs, HostPort: p.Published}}
	}
	return exposed, bindings, nil
}

// printAccessInfo は起動後の接続情報を表示する。
func printAccessInfo(svc types.ServiceConfig) {
	for _, p := range svc.Ports {
		protocol := p.Protocol
		if protocol == "" {
			protocol = defaultProtocol
		}
		fmt.Printf("ポート    localhost:%s -> コンテナ:%d/%s\n", p.Published, p.Target, protocol)
	}
	fmt.Println("接続例    ssh ubuntu@localhost -p <上のポート>   (パスワード: ubuntu)")
}

// shortID はコンテナ ID を短縮表示用に先頭 12 桁へ切り詰める。
func shortID(id string) string {
	if len(id) > shortIDLen {
		return id[:shortIDLen]
	}
	return id
}
