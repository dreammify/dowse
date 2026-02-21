# typed: strict
# frozen_string_literal: true

require_relative '../core/entity'
require_relative '../core/identifiable'
require_relative 'permission'

module Models
  class User < Core::Entity
    extend T::Sig
    include Core::Identifiable

    sig { returns(Integer) }
    attr_reader :id

    sig { returns(String) }
    attr_reader :name

    sig { returns(String) }
    attr_reader :email

    sig { returns(Permission) }
    attr_reader :permission

    sig { params(id: Integer, name: String, email: String, permission: Permission).void }
    def initialize(id, name, email, permission)
      super()
      @id = T.let(id, Integer)
      @name = T.let(name, String)
      @email = T.let(email, String)
      @permission = T.let(permission, Permission)
    end

    sig { override.returns(Core::Result) }
    def validate
      if @name.empty?
        Core::Failure.new("Name must not be blank")
      elsif !@email.include?("@")
        Core::Failure.new("Invalid email format")
      else
        Core::Success.new
      end
    end
  end
end
