#!/bin/bash
sudo apt update
sudo apt upgrade
sudo apt install  -y build-essential libopus-dev pkg-config git go shairport-sync

cd ${HOME}

git clone https://github.com/ferrousaurus/pinstrel.git
cd pinstrel

sudo cp configs/shairport-sync.conf.template /etc/shairport-sync.conf
sudo systemctl restart shairport-sync


make
sudo cp dist/pinstrel /usr/local/bin/

# Fall back to interactive prompt only if environment variables are unset or empty
if [ -z "$BOT_ID" ]; then
    read -p "Enter BOT_ID: " BOT_ID
fi

if [ -z "$USER_ID" ]; then
    read -p "Enter USER_ID: " USER_ID
fi

cat << EOF > "/etc/pinstrel.toml"
DISCORD_TOKEN = "$DISCORD_TOKEN"
DISCORD_USER_ID = "$DISCORD_USER_ID"
BITRATE = 128000
PIPE_PATH = "/tmp/shairport-sync-audio"
SOCKET_PATH = "/tmp/pinstrel.sock"
VOICE_READY_TIMEOUT = 30
EOF

sudo cp deployments/systemd/pinstrel.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable pinstrel
sudo systemctl start pinstrel
