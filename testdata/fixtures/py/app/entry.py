from .service import Service


def main() -> None:
    service = Service(2)
    service.total()


if __name__ == "__main__":
    main()
