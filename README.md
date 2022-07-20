# eth-proxy

Reverse proxy for ethereum nodes.

### Features:
- Status endpoint for all your beacon nodes
- Beacon chain API path based allow list. Allows you to restrict which API endpoints you're exposing.
- Execution JSON RPC method allow list


## Endpoints

**Status**: Shows you the configured nodes and some additional information about them.

```sh
curl http://localhost:5555/status
```

Reverse proxy to a specific node by name
```sh
# Beacon HTTP API
curl -X GET 'http://localhost:5555/proxy/beacon/node1/eth/v1/node/identity'

# Execution JSON RPC API
curl -X POST 'http://localhost:5555/proxy/execution/node1/' \
     --header 'Content-Type: application/json' --data-raw '{
        "jsonrpc":"2.0",
        "method":"eth_blockNumber",
        "params":[],
        "id":1
}'
```


### Building and running

Adjust the configuration file for your needs. An example can be seen in [example_config.yaml](example_config.yaml)


```sh
go build -o bin/eth-proxy ./cmd/eth-proxy && ./bin/eth-proxy --config example_config.yaml
```
