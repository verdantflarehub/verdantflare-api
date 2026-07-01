# verdantflare-api

<https://github.com/QuantumNous/new-api>

```bash
git remote add upstream git@github.com:QuantumNous/new-api.git

git fetch upstream

git merge v1.0.0-rc.15
```

## build

```bash
docker run -it --rm --user 1000:1000 \
  --entrypoint bash \
  -v $PWD/:/go/src/github.com/verdantflarehub/verdantflare-api \
  -w /go/src/github.com/verdantflarehub/verdantflare-api \
  registry.cn-qingdao.aliyuncs.com/wod/golang:1.24 \
  -c "
  make build
  "
```
