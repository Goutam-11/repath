from fastapi import FastAPI, APIRouter

app = FastAPI()
router = APIRouter()


@router.get("/items")
def list_items():
    return helper()


@app.route("/ping")
def ping():
    return helper()


def helper():
    return 1


def test_ping():
    assert ping() == 1


def main():
    ping()


if __name__ == "__main__":
    main()
