export async function loadUsers() {
  const response = await fetch("/api/users");
  return response.json();
}

export async function createUser() {
  return fetch("/api/users", { method: "POST" });
}

export async function addItem() {
  return fetch("/api/items", { method: "POST" });
}

export async function loadProducts() {
  return fetch("/api/products");
}

export async function missing() {
  return fetch("/api/missing");
}

export async function dynamic(id: string) {
  return fetch(`/api/users/${id}`);
}

export async function wrongMethod() {
  return fetch("/api/health", { method: "POST" });
}

export const literal = "/api/users";
