#!/bin/sh
# Like the yolks' entrypoint: run the startup command from the volume.
cd /home/container || exit 1
eval "exec ${STARTUP}"
