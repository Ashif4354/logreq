FROM node:22-alpine

WORKDIR /app

# Copy package manifest
COPY package.json ./

# Copy Node.js application source
COPY main.js ./

# Expose default port
EXPOSE 8081

# Run logreq Node.js server
ENTRYPOINT ["node", "main.js", "--host", "0.0.0.0", "--port", "8081"]
