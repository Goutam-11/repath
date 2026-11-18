declare const app: { get(path: string, handler: () => void): void };

export function products() {
  return [];
}

app.get("/api/products", products);
