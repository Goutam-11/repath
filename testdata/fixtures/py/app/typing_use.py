from .models import User
from .service import Service


def describe(name: str) -> str:
    user = User(name)
    service = Service(3)
    return service.total() + user.greet()
