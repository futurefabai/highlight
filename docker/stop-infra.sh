#!/bin/bash -ex

source env.sh

# stop the infra

if [[ "$*" == *"--hobby"* ]]; then
  CUSTOM_COMPOSE="-f compose.yml -f compose.hobby.yml"
fi
SERVICES="clickhouse collector influxdb kafka opensearch postgres redis zookeeper"
echo "Stop services: $SERVICES"
docker compose $CUSTOM_COMPOSE down --remove-orphans $SERVICES
