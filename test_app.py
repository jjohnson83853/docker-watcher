from app import image_name


def test_image_name():
    assert image_name("bullprog3/my-cool-app", "bullprog3") == "my-cool-app"
    assert image_name("bullprog3/My-App", "bullprog3") == "My-App"
    assert image_name("someorg/repo", "bullprog3") == "someorg/repo"


if __name__ == "__main__":
    test_image_name()
    print("ok")
