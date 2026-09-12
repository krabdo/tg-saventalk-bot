FROM node:24-bookworm-slim AS build
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY tsconfig.json ./
COPY src ./src
RUN npm run build

FROM node:24-bookworm-slim
LABEL org.opencontainers.image.source="https://github.com/krabdo/tg-saventalk-bot"
LABEL org.opencontainers.image.licenses="GPL-3.0"
ENV NODE_ENV=production DATA_DIR=/data
WORKDIR /app
RUN mkdir /data && chown node:node /data
COPY --from=build /app/dist ./dist
COPY package.json LICENSE ./
USER node
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s CMD ["node", "-e", "const fs=require('node:fs');process.exit(Date.now()-Number(fs.readFileSync('/data/health','utf8'))<120000?0:1)"]
CMD ["node", "dist/main.js"]
