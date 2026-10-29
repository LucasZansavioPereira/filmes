#!/usr/bin/env bash
# Instalador do Filmes: pergunta onde ficam os filmes e qual qBittorrent usar,
# escreve o .env e sobe os contêineres. Pode rodar de novo para reconfigurar.
set -euo pipefail
cd "$(dirname "$0")"

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
die()  { printf '\033[31m%s\033[0m\n' "$*" >&2; exit 1; }
ask()  { local r; read -rp "$1${2:+ [$2]}: " r; printf '%s' "${r:-${2:-}}"; }
yes_no() { local r; read -rp "$1 [s/N]: " r; [[ $r =~ ^[sSyY] ]]; }
port_busy() { ss -Hltn "sport = :$1" 2>/dev/null | grep -q .; }
# Valor para o .env: entre aspas simples, que o Docker Compose lê ao pé da letra.
envval() {
  [[ $1 != *"'"* ]] || die "A senha não pode ter aspas simples ('). Troque a senha e rode de novo."
  printf "'%s'" "$1"
}

# ---------- Pré-requisitos ----------
command -v docker >/dev/null || die "Instale o Docker primeiro: https://docs.docker.com/engine/install/"
DOCKER=(docker)
if ! docker info >/dev/null 2>&1; then
  sudo docker info >/dev/null 2>&1 || die "Não consegui falar com o Docker (nem com sudo). Ele está rodando?"
  DOCKER=(sudo docker)
fi
"${DOCKER[@]}" compose version >/dev/null 2>&1 || die "Falta o Docker Compose (pacote docker-compose-plugin)."

if [ -f .env ]; then
  yes_no "Já existe um .env. Quer configurar tudo de novo (o antigo vira .env.bak)?" || exit 0
  cp .env .env.bak
fi

PUID=${SUDO_UID:-$(id -u)}
PGID=${SUDO_GID:-$(id -g)}

# ---------- Pasta de filmes ----------
say "1. Pasta dos filmes"
echo "Onde os filmes e séries vão ficar no servidor (escolha um disco com espaço)."
PASTA=$(ask "Pasta" /srv/filmes)
PASTA=${PASTA%/}
[[ $PASTA = /* ]] || die "Use o caminho completo, começando com /."
if [ ! -d "$PASTA" ]; then
  mkdir -p "$PASTA" 2>/dev/null || { sudo mkdir -p "$PASTA" && sudo chown "$PUID:$PGID" "$PASTA"; }
  echo "Criei $PASTA."
fi

# ---------- qBittorrent ----------
say "2. qBittorrent"
if pgrep -x qbittorrent-nox >/dev/null || "${DOCKER[@]}" ps --format '{{.Image}}' | grep -qi qbittorrent; then
  echo "Parece que já tem um qBittorrent rodando neste servidor."
fi
echo "  1) Instalar um qBittorrent novo, junto com a app (o mais fácil)"
echo "  2) Usar um qBittorrent que eu já tenho"
MODO=$(ask "Opção" 1)

if [ "$MODO" = 1 ]; then
  command -v python3 >/dev/null || die "Esta opção precisa do python3 (para gerar a senha do qBittorrent)."
  QBIT_PORT=$(ask "Porta da interface web do qBittorrent" 8090)
  port_busy "$QBIT_PORT" && die "A porta $QBIT_PORT já está em uso. Rode de novo e escolha outra."
  QBIT_TORRENT_PORT=$(ask "Porta dos torrents" 6881)
  port_busy "$QBIT_TORRENT_PORT" && die "A porta $QBIT_TORRENT_PORT já está em uso. Rode de novo e escolha outra."
  if [ "$(stat -c %u "$PASTA")" != "$PUID" ]; then
    yes_no "O qBittorrent novo grava como o seu usuário. Passar a pasta $PASTA para ele?" \
      && sudo chown "$PUID:$PGID" "$PASTA"
  fi
  QBIT_USER=admin
  CONF=qbittorrent/qBittorrent/qBittorrent.conf
  if [ -f "$CONF" ]; then
    echo "O qBittorrent desta pasta já foi configurado antes."
    read -rsp "Senha do usuário admin dele: " QBIT_PASS; echo
  else
    QBIT_PASS=$(python3 -c 'import secrets; print(secrets.token_urlsafe(12))')
    # Hash no formato que o qBittorrent guarda (PBKDF2-SHA512, 100000 voltas).
    HASH=$(printf '%s' "$QBIT_PASS" | python3 -c '
import base64, hashlib, os, sys
salt = os.urandom(16)
key = hashlib.pbkdf2_hmac("sha512", sys.stdin.buffer.read(), salt, 100000)
print("@ByteArray(%s:%s)" % (base64.b64encode(salt).decode(), base64.b64encode(key).decode()))')
    mkdir -p "$(dirname "$CONF")"
    cat > "$CONF" <<EOF
[BitTorrent]
Session\\DefaultSavePath=/filmes
Session\\Port=$QBIT_TORRENT_PORT

[LegalNotice]
Accepted=true

[Preferences]
WebUI\\Address=*
WebUI\\ServerDomains=*
WebUI\\Username=$QBIT_USER
WebUI\\Password_PBKDF2="$HASH"
EOF
  fi
  ENV_QBIT="COMPOSE_PROFILES=qbittorrent
QBIT_URL=http://qbittorrent:8080
QBIT_USER=$QBIT_USER
QBIT_PASS=$(envval "$QBIT_PASS")
QBIT_PASTA=/filmes
QBIT_PORT=$QBIT_PORT
QBIT_TORRENT_PORT=$QBIT_TORRENT_PORT
PUID=$PUID
PGID=$PGID"
else
  echo "Endereço da interface web do seu qBittorrent. Se ele roda neste servidor,"
  echo "use host.docker.internal no lugar de localhost (ex.: http://host.docker.internal:8080)."
  QBIT_URL=$(ask "Endereço" http://host.docker.internal:8080)
  QBIT_URL=${QBIT_URL%/}
  QBIT_USER=$(ask "Usuário da interface web" admin)
  read -rsp "Senha da interface web: " QBIT_PASS; echo
  echo "Se o seu qBittorrent roda em Docker, diga como a pasta $PASTA aparece"
  echo "dentro do contêiner dele (ex.: /downloads/filmes). Se não, só aperte Enter."
  QBIT_PASTA=$(ask "Pasta vista pelo qBittorrent" "")

  # Testa o login daqui do servidor antes de seguir.
  TEST_URL=${QBIT_URL/host.docker.internal/localhost}
  RESP=$(curl -s -m 5 -H "Referer: $TEST_URL/" --data-urlencode "username=$QBIT_USER" \
    --data-urlencode "password=$QBIT_PASS" "$TEST_URL/api/v2/auth/login" || true)
  if [ "$RESP" = "Ok." ]; then echo "Login no qBittorrent: ok."
  elif [ -z "$RESP" ]; then warn "Não consegui falar com $TEST_URL. Confira o endereço (dá para corrigir depois no .env)."
  else warn "O qBittorrent recusou o login ($RESP). Confira usuário e senha (dá para corrigir depois no .env)."
  fi
  if [ -z "$QBIT_PASTA" ] && QUSER=$(ps -o user= -C qbittorrent-nox | head -1) && [ -n "$QUSER" ] \
     && [ "$(stat -c %U "$PASTA")" != "$QUSER" ]; then
    if yes_no "O qBittorrent roda como '$QUSER'. Dar a pasta $PASTA para esse usuário (para ele poder gravar nela)?"; then
      sudo chown "$QUSER:" "$PASTA"
    fi
  fi
  ENV_QBIT="QBIT_URL=$QBIT_URL
QBIT_USER=$QBIT_USER
QBIT_PASS=$(envval "$QBIT_PASS")
QBIT_PASTA=$QBIT_PASTA"
fi

# ---------- App ----------
say "3. App"
FILMES_PORT=$(ask "Porta da app" 8086)
port_busy "$FILMES_PORT" && ! "${DOCKER[@]}" ps --format '{{.Names}}' | grep -qx filmes \
  && die "A porta $FILMES_PORT já está em uso. Rode de novo e escolha outra."
TZ_ATUAL=$(cat /etc/timezone 2>/dev/null || timedatectl show -p Timezone --value 2>/dev/null || echo America/Sao_Paulo)
FUSO=$(ask "Fuso horário" "$TZ_ATUAL")

umask 077
cat > .env <<EOF
# Gerado pelo instalar.sh em $(date '+%Y-%m-%d %H:%M'). Veja .env.exemplo.
PASTA_FILMES=$PASTA
FILMES_PORT=$FILMES_PORT
TZ=$FUSO
$ENV_QBIT
EOF
umask 022

say "4. Subindo (a primeira vez demora uns minutos para compilar)"
"${DOCKER[@]}" compose up -d --build

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
say "Pronto!"
echo "Filmes:      http://${IP:-IP-DO-SERVIDOR}:$FILMES_PORT"
if [ "$MODO" = 1 ]; then
  echo "qBittorrent: http://${IP:-IP-DO-SERVIDOR}:$QBIT_PORT  (usuário $QBIT_USER, senha $QBIT_PASS)"
  echo "A senha também fica no arquivo .env."
fi
echo "Para ver o log: ${DOCKER[*]} compose logs -f filmes"
