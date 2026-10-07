# The egg image of the live placement suite: a shell "game" that records its
# starts and stops on the volume (game.sh).
FROM busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
COPY --chmod=0755 entrypoint.sh /entrypoint.sh
COPY --chmod=0755 game.sh /game.sh
