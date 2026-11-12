from app import Service


def start() -> int:
    service = Service(1)
    return service.total()
