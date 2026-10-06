# 前端交付形态：Node 构建出静态资源，再由 nginx 托管并反向代理 /api。
# 不用 vite preview —— 那不是交付形态，且缺少反代与缓存控制。

FROM node:24-alpine AS build
WORKDIR /app

# 依赖清单单独一层，源码变更不会击穿依赖缓存
COPY package.json package-lock.json ./
RUN npm ci

COPY . .
RUN npm run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 80
