FROM node:24-alpine
WORKDIR /app
COPY package.json package-lock.json ./
COPY apps/dashboard/package.json apps/dashboard/package.json
COPY services/control-plane/package.json services/control-plane/package.json
RUN npm ci
COPY apps/dashboard apps/dashboard
WORKDIR /app/apps/dashboard
USER node
EXPOSE 5173
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0"]
