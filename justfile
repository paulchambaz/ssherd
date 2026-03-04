dev:
  @hivemind

watch-tailwind:
  @tailwindcss -w -i static/css/main.css -o static/css/styles.css

watch-templ:
  @templ generate --watch --proxy="http://127.0.0.1:8080"

watch-go:
  @air

build:
  @tailwindcss -i static/css/main.css -o static/css/styles.css -m
  @templ generate
  @go build -o bin/ssherd ./ssherd
  @scdoc < ssherd.1.scd | sed "s/1980-01-01/$(date '+%B %Y')/" > bin/ssherd.1

test:
  @go test -cover ./...

clean:
  @rm -fr bin/

docker:
  @nix build .#docker
  @docker load < result
  @docker compose up
