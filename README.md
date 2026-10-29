# Filmes

Acervo de filmes e séries para o homelab: um único programa em Go com SQLite, que usa uns 20 a 40 MB de RAM. Quem baixa é o **qBittorrent** (um que você já tenha, ou um que o instalador sobe junto), e quem toca é o próprio navegador (ou o VLC, se preferir).

```
navegador ──► Filmes (porta 8086) ──► qBittorrent ──► pasta de filmes
                    │                                     ▲
                    └──── lê, faz capas e toca ───────────┘
```

> **Sem login.** A app não tem usuário nem senha: quem acessa a porta dela vê, baixa e apaga filmes. Deixe ela só na rede de casa (ou atrás de uma VPN como Tailscale/WireGuard). Não abra a porta no roteador.

- **Aba Baixar:** cole um link magnet (ou o endereço de um `.torrent`, ou envie o arquivo `.torrent`) e clique em **Baixar**. A app manda o qBittorrent baixar para a pasta de filmes e mostra o progresso, a velocidade e o tempo que falta. Dá para pausar, continuar e cancelar (cancelar apaga o que já foi baixado). Se o torrent já estiver no qBittorrent, aparece um aviso em vez de baixar de novo.
- **Upload:** na aba Filmes, o botão **Upload** no canto superior direito envia vídeos do seu aparelho (dá para escolher vários de uma vez) direto para a pasta de filmes, mostrando o progresso. O vídeo entra no acervo assim que o envio termina.
- **Aba Filmes:** tudo o que está na pasta de filmes, com capa (um quadro do próprio vídeo), duração e tamanho. Um download só aparece aqui quando termina. Busca, filtro por filmes, séries e não vistos, e selo "Novo" nos que chegaram há menos de 3 dias.
- **Séries:** uma pasta com vários episódios vira uma série, com os episódios em ordem por temporada (`S01E02`, `1x02`). O botão principal mostra "Continuar: T1 E3", a partir do primeiro episódio não visto.
- **Visto:** abrir um vídeo no VLC já marca ele como visto. Dá para marcar e desmarcar à mão (um por um ou a temporada toda).
- **Renomear** troca o título mostrado (o arquivo não muda). **Outra capa** tira um quadro de outro ponto do vídeo. **Apagar** exclui os arquivos do servidor e tira o torrent do qBittorrent.
- Títulos limpos a partir do nome do torrent: `The.Matrix.1999.1080p.BluRay.x264-GRP` aparece como "The Matrix", 1999. Arquivos de amostra (`sample`) ficam de fora.
- Arquivos que você colocar na pasta de filmes por conta própria também entram no acervo (a pasta é lida de novo a cada minuto).

## Assistir

O botão **▶ Assistir** toca o vídeo na própria página, com um player no estilo do AList (ArtPlayer): velocidade, tela cheia, picture-in-picture, atalhos de teclado (espaço, setas), escolha de **áudio** e **legenda**, e continua de onde você parou. Num episódio, o próximo começa sozinho quando o atual termina. Perto do fim, o vídeo é marcado como visto.

O navegador não toca a maioria dos filmes como eles vêm (MKV, áudio AC3/E-AC3/DTS). Nesses casos o servidor passa o vídeo pelo ffmpeg: a imagem é só copiada (não pesa) e só o áudio é convertido para AAC, na hora. As legendas embutidas no arquivo também aparecem; a primeira vez que um vídeo é aberto, elas levam uns segundos para ficar prontas. Só vídeos com imagem em formato que o navegador não toca (HEVC/x265, XviD, MPEG-2) ficam de fora, porque converter a imagem seria pesado demais para o servidor: esses abrem num player externo.

Abaixo do player há botões para abrir em outro player, como no AList: VLC em todos os aparelhos, e também MX Player (Android), Infuse e nPlayer (iPhone/iPad), IINA (Mac) e PotPlayer (Windows).

### No VLC

- **Android:** abre o app do VLC direto. Se ele não estiver instalado, o celular leva para a Play Store.
- **iPhone e iPad:** abre o app VLC for Mobile (instale pela App Store).
- **Computador:** o botão baixa uma playlist `.m3u` pequena. Na primeira vez, abra esse arquivo com o VLC e marque para sempre abrir assim (no Chrome: seta ao lado do download › "Sempre abrir arquivos deste tipo"). Em séries, a playlist já traz os episódios seguintes, um atrás do outro.
- **TV ou outro aparelho:** o botão **Copiar link** copia o endereço do vídeo. No VLC: Mídia › Abrir fluxo de rede, e cole.

## Instalação

### Do que você precisa

- Um servidor Linux com **Docker** e o **Docker Compose** ([como instalar](https://docs.docker.com/engine/install/)). Funciona em máquina fraca: a app fica limitada a 256 MB de RAM e 1 CPU.
- Um disco com espaço para os filmes.
- `git`, `curl` e `python3` (já vêm na maioria das distribuições).

### Jeito fácil: o instalador

```sh
git clone https://github.com/LucasZansavioPereira/filmes.git
cd filmes
./instalar.sh
```

Ele pergunta:

1. **A pasta dos filmes** no servidor (ex.: `/srv/filmes`). Se ela não existir, ele cria.
2. **Qual qBittorrent usar:**
   - **Instalar um novo junto com a app:** sobe o qBittorrent ([imagem da linuxserver.io](https://docs.linuxserver.io/images/docker-qbittorrent/)) no mesmo `docker compose`, já configurado para salvar na pasta de filmes, com uma senha gerada na hora. Não precisa mexer em nada nele.
   - **Usar um que você já tem:** você informa o endereço da interface web, o usuário e a senha, e o instalador testa o login antes de seguir. Veja [os detalhes abaixo](#usando-um-qbittorrent-que-você-já-tem).
3. **A porta da app** (padrão 8086) e o fuso horário.

No fim ele grava tudo no `.env`, sobe os contêineres e mostra os endereços:

```
Filmes:      http://192.168.0.10:8086
qBittorrent: http://192.168.0.10:8090  (usuário admin, senha ...)
```

A primeira subida demora uns minutos, porque compila a app. Para conferir se a app achou o qBittorrent:

```sh
docker compose logs -f filmes
```

Deve aparecer `qBittorrent v5.x.x em http://..., salvando em ...`. (Ctrl+C sai do log; a app continua rodando.)

### Jeito manual

```sh
cp .env.exemplo .env
nano .env
docker compose up -d --build
```

O `.env.exemplo` explica cada variável:

| Variável | O que é |
|---|---|
| `PASTA_FILMES` | Pasta do servidor onde ficam os filmes e onde o qBittorrent salva. |
| `FILMES_PORT` | Porta da app no servidor (padrão 8086). |
| `TZ` | Fuso horário (padrão `America/Sao_Paulo`). |
| `QBIT_URL` | Endereço da interface web do qBittorrent, visto **de dentro do contêiner** da app. |
| `QBIT_USER`, `QBIT_PASS` | Usuário e senha da interface web do qBittorrent. |
| `QBIT_PASTA` | A pasta de filmes como o qBittorrent enxerga. Só muda se ele roda em Docker com outro caminho. |
| `COMPOSE_PROFILES` | `qbittorrent` para subir o qBittorrent junto com a app. |
| `QBIT_PORT`, `QBIT_TORRENT_PORT` | Com o qBittorrent junto: porta da interface web dele (padrão 8090) e dos torrents (padrão 6881). |
| `PUID`, `PGID` | Com o qBittorrent junto: dono dos arquivos baixados (`id -u` e `id -g`). |

Se escolher o qBittorrent junto no jeito manual, a senha do `.env` precisa ser a mesma da interface dele: suba uma vez, pegue a senha temporária com `docker compose logs qbittorrent`, entre em `http://IP:8090`, troque a senha em Ferramentas › Opções › Interface Web e coloque a nova em `QBIT_PASS`. (O instalador faz isso sozinho.)

### Usando um qBittorrent que você já tem

A app conversa com o qBittorrent pela **interface web** (a API dela), então ela precisa estar ligada: no qBittorrent, Ferramentas › Opções › Interface Web.

**Ele roda direto no servidor** (o `qbittorrent-nox`, sem Docker):

- `QBIT_URL=http://host.docker.internal:8080`: `host.docker.internal` é o próprio servidor visto de dentro do contêiner. Troque 8080 pela porta da interface web dele.
- `QBIT_PASTA` fica vazio.
- O usuário que roda o qBittorrent precisa poder gravar na pasta de filmes. Descubra quem é com `ps -o user= -C qbittorrent-nox` e rode `sudo chown ESSE_USUARIO: /srv/filmes`. O instalador oferece fazer isso.

**Ele roda em Docker** (outro `docker compose`):

1. Veja a porta e as pastas dele:
   ```sh
   docker ps --format '{{.Names}}  {{.Ports}}' | grep -i qbit
   docker inspect NOME_DO_CONTEINER --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}'
   ```
2. Use como `PASTA_FILMES` uma pasta **dentro** da pasta de downloads dele. Exemplo: se aparecer `/srv/torrents -> /downloads`, crie `/srv/torrents/filmes` e use `PASTA_FILMES=/srv/torrents/filmes` e `QBIT_PASTA=/downloads/filmes`.
3. `QBIT_URL=http://host.docker.internal:PORTA`, com a porta que ele publica no servidor.

**Ele roda em outra máquina:** `QBIT_URL=http://IP_DA_MAQUINA:8080`. A pasta de filmes precisa ser a mesma nas duas máquinas (um compartilhamento NFS/SMB montado nos dois lados), e `QBIT_PASTA` é o caminho dela na máquina do qBittorrent.

Os downloads feitos pela app ficam na categoria **filmes** do qBittorrent, então você também vê e controla eles pela interface dele. Quando um download termina, o qBittorrent continua compartilhando (semeando) o arquivo, como de costume; o botão **Tirar da lista** na aba Baixar para de compartilhar sem apagar o filme.

### Sem qBittorrent

Também funciona: deixe `QBIT_URL` vazio. A aba Baixar fica desligada, e a app serve só para organizar e assistir o que estiver na pasta (copiado por você ou enviado pelo botão **Upload**).

## Onde ficam as coisas

```
PASTA_FILMES/          os filmes e séries, do jeito que foram baixados ou enviados
data/filmes.db         banco SQLite (títulos, o que já foi visto)
data/thumbs/           capas
data/subs/             legendas já extraídas dos vídeos
qbittorrent/           configuração do qBittorrent (só se ele roda junto)
.env                   suas configurações e senhas (não vai para o git)
```

Para fazer backup, basta guardar `data/`, `qbittorrent/` e `.env`. Os filmes são arquivos comuns na `PASTA_FILMES`.

## Atualizar

```sh
git pull
docker compose up -d --build
```

O `.env`, a pasta `data/` e a pasta `qbittorrent/` são mantidos.

## Problemas comuns

| Sintoma | O que fazer |
|---|---|
| Aba Baixar diz que o qBittorrent não está configurado | `QBIT_URL` está vazio no `.env`. |
| Log diz "não consegui falar com o qBittorrent" | Endereço errado. Lembre que, de dentro do contêiner, `localhost` é o próprio contêiner: use `host.docker.internal` ou o IP do servidor. |
| Log diz que o usuário ou a senha estão errados | Confira `QBIT_USER` e `QBIT_PASS` e rode `docker compose up -d`. Depois de muitas tentativas erradas, o qBittorrent bloqueia o login por um tempo. |
| O download começa mas dá erro de permissão | O qBittorrent não consegue gravar na `PASTA_FILMES`. Veja o dono da pasta (`ls -ld`) e use `chown`. |
| O filme baixado não aparece na aba Filmes | Ele só aparece quando o download termina. Se `QBIT_PASTA` estiver errado, o qBittorrent salva em outro lugar: confira o caminho na interface dele. |
| Vídeo não toca no navegador | Vídeos em HEVC/x265, XviD ou MPEG-2 não tocam no navegador; use o botão do VLC. |

## Desenvolvimento

```sh
go run .            # abre em http://localhost:8080, dados em ./data, filmes em ./filmes
go test ./...
```

Variáveis: `FILMES_DATA`, `FILMES_PASTA`, `FILMES_ADDR`, `QBIT_URL`, `QBIT_USER`, `QBIT_PASS`, `QBIT_PASTA` (padrão: igual a `FILMES_PASTA`) e `QBIT_CATEGORIA` (padrão `filmes`).

O player é o [ArtPlayer](https://github.com/zhw2590582/ArtPlayer) (MIT), incluído em `web/artplayer.js`.

## Licença

MIT. Veja [LICENSE](LICENSE).
