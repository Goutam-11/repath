from fastapi import FastAPI

app = FastAPI()


@app.get("/api/users")
def list_users():
    return []


@app.post("/api/users")
def create_user():
    return {}


@app.get("/api/health")
def health():
    return {"ok": True}
